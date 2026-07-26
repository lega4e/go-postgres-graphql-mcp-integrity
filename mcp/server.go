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
// The server is read-only by construction: the compiler emits nothing but a
// SELECT over GRAPH_TABLE, there is no migration or mutation tool, and the
// binary opens its pool with default_transaction_read_only=on so even a bug
// that emitted a write would be refused by the database (design D4).
//
// The compiled SQL is not part of either tool's result: the server connects to
// Postgres, executes, and returns the data (design D2a).
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/sdl"
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
	mcp       *mcpsdk.Server
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
func New(doc *sdl.Document, sdlSource string, db exec.Querier, opts ...Option) *Server {
	cfg := config{name: "gopgql", version: "dev"}
	for _, opt := range opts {
		opt(&cfg)
	}

	s := &Server{
		doc:       doc,
		sdlSource: sdlSource,
		db:        db,
		comp:      compiler.New(doc, cfg.compilerOpts...),
		intro:     newIntrospector(doc, sdlSource),
	}
	s.mcp = mcpsdk.NewServer(&mcpsdk.Implementation{Name: cfg.name, Version: cfg.version}, nil)
	s.register()
	return s
}

// MCPServer returns the underlying MCP server, for a caller that wants to add
// middleware or drive the connection itself.
func (s *Server) MCPServer() *mcpsdk.Server { return s.mcp }

// Run serves MCP over the transport until the context is cancelled or the peer
// disconnects.
func (s *Server) Run(ctx context.Context, t mcpsdk.Transport) error {
	return s.mcp.Run(ctx, t)
}

const introspectDescription = `Discover what this GraphQL schema exposes, before querying it.

Runs a standard GraphQL introspection query over the schema this server was started with and returns its result:

  - no arguments: the overview — every queryable root field with its arguments, and the name and kind of every type, with the types' own field definitions omitted so the result stays small.
  - type: "<TypeName>": that type's ` + "`__type`" + ` detail — its fields, each field's type reference, and which fields lead to another type.
  - full: true: the complete introspection result (` + "`__schema`" + ` with every type fully expanded).
  - format: "sdl": the schema as an SDL document.

Start with no arguments, then drill into one type. The same information is reachable through the query tool with the ` + "`__schema`" + ` and ` + "`__type`" + ` meta-fields.`

const queryDescription = `Run a GraphQL query against the mapped PostgreSQL database and return the data.

Only query operations are supported; the mapped graph is read-only. Values must travel in ` + "`variables`" + `, which are bound as SQL parameters.

Discovery goes through the same tool: this schema answers the standard GraphQL introspection meta-fields ` + "`__schema`" + `, ` + "`__type(name:)`" + ` and ` + "`__typename`" + `, served from the schema without touching the database. Send this verbatim to see what is queryable:

  { __schema { queryType { name fields { name args { name } type { kind name ofType { kind name } } } } types { kind name } } }

then narrow to one type:

  { __type(name: "TypeName") { name fields { name type { kind name ofType { kind name } } } } }

and finally select its fields:

  { rootField(someScalar: "value") { id someScalar relatedField { id } } }

format: "json" (default) returns the nested response. format: "markdown" renders a table instead, and is refused for an operation that selects a relationship, because a table cannot represent nesting.`

// register declares both tools with the input schemas and descriptions a client
// needs to call them without guessing — including how to introspect, so an
// agent that has only the tool list can still reach a valid data query.
func (s *Server) register() {
	s.mcp.AddTool(&mcpsdk.Tool{
		Name:        ToolIntrospect,
		Description: introspectDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type": map[string]any{
					"type":        "string",
					"description": "Introspect this type by name (`__type(name:)`). Omit for the schema overview.",
				},
				"full": map[string]any{
					"type":        "boolean",
					"description": "Return the complete introspection result rather than the overview.",
				},
				"format": map[string]any{
					"type":        "string",
					"enum":        []any{FormatIntrospection, FormatSDL},
					"description": "`introspection` (default) returns the introspection result; `sdl` returns the schema document.",
				},
			},
			"additionalProperties": false,
		},
	}, s.handleIntrospect)

	s.mcp.AddTool(&mcpsdk.Tool{
		Name:        ToolQuery,
		Description: queryDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The GraphQL query to execute. Introspection meta-fields are answered from the schema.",
				},
				"variables": map[string]any{
					"type":        "object",
					"description": "Values for the operation's variables. They are bound as SQL parameters, never interpolated.",
				},
				"format": map[string]any{
					"type":        "string",
					"enum":        []any{FormatJSON, FormatMarkdown},
					"description": "`json` (default) returns the nested response; `markdown` renders a table, and is refused for nested selections.",
				},
			},
			"required":             []any{"query"},
			"additionalProperties": false,
		},
	}, s.handleQuery)
}

type introspectArgs struct {
	Type   string `json:"type"`
	Full   bool   `json:"full"`
	Format string `json:"format"`
}

type queryArgs struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
	Format    string         `json:"format"`
}

func (s *Server) handleIntrospect(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	var args introspectArgs
	if err := decodeArgs(req.Params.Arguments, &args); err != nil {
		return toolError(err), nil
	}
	out, err := s.Introspect(args.Type, args.Full, args.Format)
	if err != nil {
		return toolError(err), nil
	}
	return toolText(out), nil
}

func (s *Server) handleQuery(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	var args queryArgs
	if err := decodeArgs(req.Params.Arguments, &args); err != nil {
		return toolError(err), nil
	}
	if args.Query == "" {
		return toolError(fmt.Errorf("gopgql/mcp: query is required")), nil
	}
	out, err := s.Query(ctx, args.Query, args.Variables, args.Format)
	if err != nil {
		return toolError(err), nil
	}
	return toolText(out), nil
}

// Introspect answers the introspect tool: it issues one of four standard
// introspection queries against the loaded schema. It never touches the
// database.
func (s *Server) Introspect(typeName string, full bool, format string) (string, error) {
	switch format {
	case "", FormatIntrospection:
	case FormatSDL:
		return s.sdlSource, nil
	default:
		return "", fmt.Errorf("gopgql/mcp: unknown format %q; supported formats are %q and %q", format, FormatIntrospection, FormatSDL)
	}

	switch {
	case typeName != "":
		return s.Query(context.Background(), typeDetailQuery, map[string]any{"name": typeName}, FormatJSON)
	case full:
		return s.Query(context.Background(), FullIntrospectionQuery, nil, FormatJSON)
	default:
		return s.Query(context.Background(), overviewQuery, nil, FormatJSON)
	}
}

// decodeArgs unmarshals the raw tool arguments, rejecting anything the schema
// does not declare so a typo surfaces as an error rather than a silent default.
func decodeArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("gopgql/mcp: invalid arguments: %w", err)
	}
	return nil
}

func toolText(text string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}}}
}

// toolError reports a failure as a tool error rather than a protocol error, so
// the agent sees the message and can correct itself (design D5).
func toolError(err error) *mcpsdk.CallToolResult {
	res := &mcpsdk.CallToolResult{}
	res.SetError(err)
	return res
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
