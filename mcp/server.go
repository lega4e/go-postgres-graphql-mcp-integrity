// Package mcp serves a gopgql schema and its database over the Model Context
// Protocol, so an agent can discover what is queryable and query it.
//
// Two tools, matching the shape blurrah/mcp-graphql established (design D1):
//
//   - introspect — issues a standard GraphQL introspection query on the
//     caller's behalf, defaulting to a selection small enough to read.
//   - query — compiles a GraphQL operation, executes it against the connected
//     database, and returns the nested response.
//
// The server is read-only by construction, and since M11 that is a decision
// rather than a consequence. gopgql can compile a mutation — a @function field
// maps to a call on a PL/pgSQL function the database owns — but only through a
// handle the *caller* supplies (SPEC.md §7 → M11). This server has no such
// handle: it opens its own pool, with default_transaction_read_only=on, and
// exposes no mutation tool and no migration tool. An agent able to enqueue work
// is a different authorization question from an agent able to read one, and
// answering it is not this server's to do quietly (design D4).
//
// The compiled SQL is not part of either tool's result: the server connects to
// Postgres, executes, and returns the data (design D2a).
//
// # The tools ride on goga/mcp
//
// Both tools are registered through github.com/lega4e/goga/mcp.AddTool,
// which is that module's only route onto the wrapped SDK server. Everything a
// tool call needs around it comes with the route rather than being written
// here: a goga.mcp.tool span per call, a bound on how long one may run, a
// panic recovered into a result instead of a dead process, and the caller's
// trace context restored from the request's _meta. The operation spans this
// package opens — Introspect, Query — sit inside that one, so a trace reads
// tool call, then what the tool did, then what the database did.
//
// A tool here returns an ordinary error and goga reports it in band, with
// IsError set on the result, which is what the specification asks for: the
// model reads the failure and corrects itself rather than seeing a server that
// looks broken.
//
// Each tool's argument and result types are plain Go structs and the SDK
// derives their JSON schemas, so the schema a client reads and the decoding
// the tool runs cannot drift. That is also why both results are structs: goga
// derives the call's structured content from the tool's output value, so a
// pre-rendered document would arrive as one JSON-escaped string.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gogamcp "github.com/lega4e/goga/mcp"
	"github.com/lega4e/goga/telemetry"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/sdl"
)

// module is the goga module name this package's telemetry is attributed to, and
// instr the handle it is emitted through. The handle resolves through
// OpenTelemetry's globals on every use, so taking it at package level — before
// the composition root has configured anything — is deliberate and safe.
const module = "mcp"

var instr = telemetry.For(module)

// The attribute keys this package records. Constants, not literals at the call
// site, so one key cannot drift into two spellings.
const (
	// attrFormat is the result format the caller asked for.
	attrFormat = attribute.Key("gopgql.format")
	// attrIntrospectType is the type name an introspect call drilled into, and
	// is absent from the overview and full-schema calls.
	attrIntrospectType = attribute.Key("gopgql.introspect.type")
)

// The tool names the server advertises.
const (
	ToolIntrospect = "introspect"
	ToolQuery      = "query"
)

// The formats the introspect tool accepts.
const (
	FormatIntrospection = "introspection"
	FormatSDL           = "sdl"
)

// Server exposes one SDL document and one database connection over MCP.
type Server struct {
	doc       *sdl.Document
	sdlSource string
	db        exec.Querier
	comp      *compiler.Compiler
	intro     *introspector
	mcp       *gogamcp.Server
}

type config struct {
	name         string
	version      string
	compilerOpts []compiler.Option
}

// Option configures a Server.
type Option func(*config)

// WithName overrides the implementation name the server reports.
func WithName(name string) Option { return func(c *config) { c.name = name } }

// WithVersion overrides the implementation version the server reports.
func WithVersion(v string) Option { return func(c *config) { c.version = v } }

// WithCompilerOptions passes options through to the compiler — the graph name
// and the depth ceiling.
func WithCompilerOptions(opts ...compiler.Option) Option {
	return func(c *config) { c.compilerOpts = append(c.compilerOpts, opts...) }
}

// New builds a server over a parsed SDL document and a database handle, and
// registers both tools. sdlSource is the verbatim document, returned by the
// introspect tool's SDL format.
//
// It returns an error because goga/mcp validates its options at construction:
// a server that cannot be built is a startup failure, not a tool call that
// fails later.
func New(doc *sdl.Document, sdlSource string, db exec.Querier, opts ...Option) (*Server, error) {
	cfg := config{name: "gopgql", version: "dev"}
	for _, opt := range opts {
		opt(&cfg)
	}

	m, err := gogamcp.New(gogamcp.WithName(cfg.name), gogamcp.WithVersion(cfg.version))
	if err != nil {
		return nil, fmt.Errorf("gopgql/mcp: %w", err)
	}

	s := &Server{
		doc:       doc,
		sdlSource: sdlSource,
		db:        db,
		comp:      compiler.New(doc, cfg.compilerOpts...),
		intro:     newIntrospector(doc, sdlSource),
		mcp:       m,
	}
	s.register()
	return s, nil
}

// Handler serves this server over streamable HTTP, for a process that mounts
// MCP beside its other routes on one port — which is what cmd/gopgql-mcp does
// under --transport http, handing the mux to goga/serve.
func (s *Server) Handler() http.Handler { return s.mcp.Handler() }

// Run serves MCP over goga's configured transport — stdio, which is how an
// agent spawns a server it owns — until the context is cancelled or the peer
// disconnects.
func (s *Server) Run(ctx context.Context) error { return s.mcp.Run(ctx) }

// Connect serves one session over a transport the caller already holds, and
// returns once the session is established.
//
// It is the one place gopgql reaches through goga/mcp's escape hatch, and it
// is here because goga resolves a transport by *name* from a registry: that
// covers stdio and a listening transport, and has nothing to say about an
// in-process transport pair a caller constructed itself, which is what the
// integration suite drives the server with. Nothing is registered through the
// escape hatch — both tools go on through goga's AddTool — so the span, the
// timeout and the panic guard apply to a session opened this way exactly as
// they do to one goga opened.
func (s *Server) Connect(ctx context.Context, t mcpsdk.Transport) (*mcpsdk.ServerSession, error) {
	return s.mcp.SDK().Connect(ctx, t, nil)
}

const introspectDescription = `Discover what this GraphQL schema exposes, before querying it.

Runs a standard GraphQL introspection query over the schema this server was started with and returns its result:

  - no arguments: the overview — every queryable root field with its arguments, and the name and kind of every type, with the types' own field definitions omitted so the result stays small.
  - type: "<TypeName>": that type's ` + "`__type`" + ` detail — its fields, each field's type reference, and which fields lead to another type.
  - full: true: the complete introspection result (` + "`__schema`" + ` with every type fully expanded).
  - format: "sdl": the schema as an SDL document.

Start with no arguments, then drill into one type. The same information is reachable through the query tool with the ` + "`__schema`" + ` and ` + "`__type`" + ` meta-fields.

The arguments are ranked, so supplying more than one is never ambiguous: ` + "`format: \"sdl\"`" + ` wins over everything, then ` + "`type`" + `, then ` + "`full`" + `. Any other value of ` + "`format`" + ` is an error rather than a silent default.`

const queryDescription = `Run a GraphQL query against the mapped PostgreSQL database and return the data.

Only query operations are supported by this tool. Values must travel in ` + "`variables`" + `, which are bound as SQL parameters.

Discovery goes through the same tool: this schema answers the standard GraphQL introspection meta-fields ` + "`__schema`" + `, ` + "`__type(name:)`" + ` and ` + "`__typename`" + `, served from the schema without touching the database. Send this verbatim to see what is queryable:

  { __schema { queryType { name fields { name args { name } type { kind name ofType { kind name } } } } types { kind name } } }

then narrow to one type:

  { __type(name: "TypeName") { name fields { name type { kind name ofType { kind name } } } } }

and finally select its fields:

  { rootField(someScalar: "value") { id someScalar relatedField { id } } }

format: "json" (default) returns the nested response. format: "markdown" renders a table instead, and is refused for an operation that selects a relationship, because a table cannot represent nesting.`

// register declares both tools on goga/mcp, which is what attaches the span,
// the per-tool timeout and the panic guard to every call — and it is the only
// route onto the wrapped server, so none of the three can be skipped.
//
// The schemas are no longer written out here: the SDK derives each one from
// the tool's Go argument type, so the declaration a client reads and the
// decoding the tool runs on cannot drift apart. The descriptions stay, because
// nothing can derive those — including how to introspect, so an agent that has
// only the tool list can still reach a valid data query.
func (s *Server) register() {
	gogamcp.AddTool(s.mcp, ToolIntrospect, introspectDescription, s.introspectTool)
	gogamcp.AddTool(s.mcp, ToolQuery, queryDescription, s.queryTool)
}

// IntrospectInput is the introspect tool's argument object.
//
// The arguments are ranked rather than exclusive — see introspectDescription —
// so a caller supplying more than one is answered rather than refused.
type IntrospectInput struct {
	// Type drills into one type by name (`__type(name:)`).
	Type string `json:"type,omitempty" jsonschema:"introspect this type by name (__type(name:)); omit for the schema overview"`
	// Full asks for the complete introspection result.
	Full bool `json:"full,omitempty" jsonschema:"return the complete introspection result rather than the overview"`
	// Format selects the introspection result or the SDL document.
	Format string `json:"format,omitempty" jsonschema:"introspection (default) returns the introspection result; sdl returns the schema document"`
}

// IntrospectOutput is the introspect tool's result. Exactly one field is set:
// the format decides which.
//
// It is a struct rather than the rendered string the tool used to return
// because goga/mcp derives a tool's result from its Go output type — the SDK
// puts it in the call's structured content and mirrors it as text — so a
// pre-rendered document would reach the caller as one JSON-escaped string
// instead of as a result it can read.
type IntrospectOutput struct {
	// Schema is the introspection result, for the introspection format.
	Schema map[string]any `json:"schema,omitempty" jsonschema:"the GraphQL introspection result"`
	// SDL is the schema document, for the sdl format.
	SDL string `json:"sdl,omitempty" jsonschema:"the schema as an SDL document, returned for format sdl"`
}

// QueryInput is the query tool's argument object.
type QueryInput struct {
	// Query is the GraphQL operation to run. It is the one required argument.
	Query string `json:"query" jsonschema:"the GraphQL query to execute; introspection meta-fields are answered from the schema"`
	// Variables carries the operation's values, bound as SQL parameters.
	Variables map[string]any `json:"variables,omitempty" jsonschema:"values for the operation's variables; they are bound as SQL parameters, never interpolated"`
	// Format selects the nested response or a markdown table.
	Format string `json:"format,omitempty" jsonschema:"json (default) returns the nested response; markdown renders a table, and is refused for nested selections"`
}

// QueryOutput is the query tool's result. Exactly one field is set: the format
// decides which. See [IntrospectOutput] for why it is a struct.
type QueryOutput struct {
	// Data is the shaped GraphQL response, for the json format.
	Data map[string]any `json:"data,omitempty" jsonschema:"the nested GraphQL response, keyed by response key"`
	// Table is the rendered table, for the markdown format.
	Table string `json:"table,omitempty" jsonschema:"the result rendered as a markdown table, returned for format markdown"`
}

// introspectTool is the introspect tool.
//
// It returns an ordinary error: goga/mcp converts one into an in-band tool
// result with IsError set, which is what the specification asks for and what
// lets the model read the failure and correct itself.
func (s *Server) introspectTool(ctx context.Context, in IntrospectInput) (IntrospectOutput, error) {
	res, err := s.introspectResult(ctx, in.Type, in.Full, in.Format)
	if err != nil {
		return IntrospectOutput{}, err
	}
	return IntrospectOutput{Schema: res.data, SDL: res.text}, nil
}

// queryTool is the query tool. Like [Server.introspectTool] it reports a
// failure as an ordinary error.
func (s *Server) queryTool(ctx context.Context, in QueryInput) (QueryOutput, error) {
	if in.Query == "" {
		return QueryOutput{}, errors.New("gopgql/mcp: query is required")
	}
	res, err := s.queryResult(ctx, in.Query, in.Variables, in.Format)
	if err != nil {
		return QueryOutput{}, err
	}
	return QueryOutput{Data: res.data, Table: res.text}, nil
}

// Introspect answers the introspect tool in its rendered form: it issues one
// of four standard introspection queries against the loaded schema and returns
// the result as text. It never touches the database.
func (s *Server) Introspect(ctx context.Context, typeName string, full bool, format string) (string, error) {
	res, err := s.introspectResult(ctx, typeName, full, format)
	if err != nil {
		return "", err
	}
	return res.render()
}

// introspectResult is [Server.Introspect] without the rendering, and is what
// the tool calls: goga/mcp turns a tool's Go output value into the call's
// structured content, so the tool wants the response rather than a document.
//
// The context is the calling tool invocation's, so an introspection is a child
// of the call that asked for it rather than a trace of its own.
//
// The result parameters are named because the deferred closer observes the
// error *variable*; the work itself is delegated so that no `return` inside it
// can bypass the assignment.
func (s *Server) introspectResult(ctx context.Context, typeName string, full bool, format string) (res result, err error) {
	attrs := []attribute.KeyValue{attrFormat.String(defaultFormat(format, FormatIntrospection))}
	if typeName != "" {
		attrs = append(attrs, attrIntrospectType.String(typeName))
	}
	ctx, end := instr.Start(ctx, "Introspect", attrs...)
	defer func() { end(err) }()

	res, err = s.introspect(ctx, typeName, full, format)
	return res, err
}

func (s *Server) introspect(ctx context.Context, typeName string, full bool, format string) (result, error) {
	switch format {
	case "", FormatIntrospection:
	case FormatSDL:
		return result{text: s.sdlSource}, nil
	default:
		return result{}, fmt.Errorf("gopgql/mcp: unknown format %q; supported formats are %q and %q", format, FormatIntrospection, FormatSDL)
	}

	switch {
	case typeName != "":
		return s.queryResult(ctx, typeDetailQuery, map[string]any{"name": typeName}, FormatJSON)
	case full:
		return s.queryResult(ctx, FullIntrospectionQuery, nil, FormatJSON)
	default:
		return s.queryResult(ctx, overviewQuery, nil, FormatJSON)
	}
}

// defaultFormat names the format an empty argument selects, so that a span
// records the format that ran rather than the absence of one.
func defaultFormat(format, fallback string) string {
	if format == "" {
		return fallback
	}
	return format
}

// overviewQuery is the default introspection: every root field with its
// arguments, and every type by name and kind — without the types' own field
// definitions, which is what keeps the result affordable on a large schema.
const overviewQuery = `query Overview {
  __schema {
    queryType {
      name
      fields {
        name
        description
        args { name description type { ...TypeRef } }
        type { ...TypeRef }
      }
    }
    types { kind name description }
  }
}
fragment TypeRef on __Type {
  kind
  name
  ofType { kind name ofType { kind name ofType { kind name } } }
}`

// typeDetailQuery is the drill-down: one type's fields and their type
// references, so an agent can see which fields lead to another type.
const typeDetailQuery = `query TypeDetail($name: String!) {
  __type(name: $name) {
    kind
    name
    description
    fields {
      name
      description
      args { name description type { ...TypeRef } }
      type { ...TypeRef }
    }
    interfaces { name }
    possibleTypes { name }
    enumValues { name description }
  }
}
fragment TypeRef on __Type {
  kind
  name
  ofType { kind name ofType { kind name ofType { kind name } } }
}`

// FullIntrospectionQuery is the introspection query GraphQL clients send. It is
// exported so a test can assert the server answers the real thing rather than a
// hand-picked selection.
const FullIntrospectionQuery = `query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types { ...FullType }
    directives {
      name
      description
      locations
      args { ...InputValue }
    }
  }
}
fragment FullType on __Type {
  kind
  name
  description
  fields(includeDeprecated: true) {
    name
    description
    args { ...InputValue }
    type { ...TypeRef }
    isDeprecated
    deprecationReason
  }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) {
    name
    description
    isDeprecated
    deprecationReason
  }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue {
  name
  description
  type { ...TypeRef }
  defaultValue
}
fragment TypeRef on __Type {
  kind
  name
  ofType {
    kind
    name
    ofType {
      kind
      name
      ofType {
        kind
        name
        ofType {
          kind
          name
          ofType {
            kind
            name
            ofType {
              kind
              name
              ofType { kind name }
            }
          }
        }
      }
    }
  }
}`
