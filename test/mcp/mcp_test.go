// Package mcp_test is the MCP integration suite. It boots a real
// postgres:19beta2 container, applies a generated migration, seeds a follow
// graph, and drives the server with a **real** SDK client — over an in-memory
// transport pair for most scenarios, and over stdio against the **built
// binary** for one, so cmd/gopgql-mcp's wiring is covered too (design D7).
//
// It proves the three capabilities the change adds:
//
//   - standard GraphQL introspection over the loaded schema, including the
//     introspection query a real client sends, and served without touching the
//     database;
//   - a query tool that compiles, binds its values as parameters, executes and
//     shapes the response, and reports compile and database failures as tool
//     errors without dying;
//   - a read-only server: no migration or mutation tool, and a connection the
//     database itself refuses writes on.
package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	oexec "os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	// Registers the "pgx" database/sql driver used by goose and by
	// postgres.WithSQLDriver for Snapshot/Restore.
	_ "github.com/jackc/pgx/v5/stdlib"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pressly/goose/v3"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	gopgqlmcp "github.com/lega4e/gopgql/mcp"
	"github.com/lega4e/gopgql/migrate"
	"github.com/lega4e/gopgql/schema"
	"github.com/lega4e/gopgql/sdl"
)

// A single container is shared across the suite; the baseline snapshot is the
// empty database, restored before every scenario (mirrors the M1–M5 suites).
var (
	pgc        *postgres.PostgresContainer
	connString string
	// binaryPath is the built cmd/gopgql-mcp, for the stdio scenario.
	binaryPath string
)

// TestFeatures is the godog entry point under `go test`. It always runs against
// a real postgres:19beta2 container — there is no skip path (SPEC.md §10).
func TestFeatures(t *testing.T) {
	ctx := context.Background()

	var err error
	pgc, err = postgres.Run(ctx, "postgres:19beta2",
		postgres.WithDatabase("gopgql"),
		postgres.WithUsername("gopgql"),
		postgres.WithPassword("gopgql"),
		postgres.WithSQLDriver("pgx"),
		tc.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3*time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start postgres:19beta2 container: %v", err)
	}
	t.Cleanup(func() {
		if err := pgc.Terminate(context.Background()); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	if err := pgc.Snapshot(ctx); err != nil {
		t.Fatalf("snapshot baseline: %v", err)
	}

	connString, err = pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose set dialect: %v", err)
	}
	goose.SetLogger(goose.NopLogger())

	binaryPath = buildBinary(t)

	suite := godog.TestSuite{
		Name:                "mcp",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("mcp feature scenarios failed")
	}
}

// buildBinary compiles cmd/gopgql-mcp so a scenario can spawn the real thing
// over stdio rather than only the library.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "gopgql-mcp")
	cmd := oexec.Command("go", "build", "-o", out, "github.com/lega4e/gopgql/cmd/gopgql-mcp")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/gopgql-mcp: %v\n%s", err, combined)
	}
	return out
}

// statementTracer records every statement the server's pool executes, so a
// scenario can assert that a value travelled as a bind parameter — and that a
// rejected operation, or an introspection query, sent nothing at all.
type statementTracer struct {
	mu         sync.Mutex
	statements []string
	args       [][]any
}

func (tr *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.statements = append(tr.statements, data.SQL)
	tr.args = append(tr.args, data.Args)
	return ctx
}

func (tr *statementTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (tr *statementTracer) reset() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.statements = nil
	tr.args = nil
}

func (tr *statementTracer) recorded() ([]string, [][]any) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.statements...), append([][]any(nil), tr.args...)
}

// scenarioState carries per-scenario state between steps.
type scenarioState struct {
	// seed writes the fixtures; the server never gets this pool.
	seed *pgxpool.Pool
	// ro is the pool the server executes on: read-only, and traced.
	ro     *pgxpool.Pool
	tracer *statementTracer

	doc       *sdl.Document
	sdlSource string
	model     *schema.Schema
	dirs      []string

	server  *gopgqlmcp.Server
	session *mcpsdk.ClientSession
	// binary is the client session for the stdio scenario, when there is one.
	binary *mcpsdk.ClientSession

	vars   map[string]any
	tools  []*mcpsdk.Tool
	result *mcpsdk.CallToolResult
	text   string
}

// InitializeScenario resets the database, opens the two pools, and registers
// steps.
func InitializeScenario(sc *godog.ScenarioContext) {
	st := &scenarioState{}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		if err := pgc.Restore(ctx); err != nil {
			return ctx, fmt.Errorf("restore snapshot: %w", err)
		}
		pool, err := pgxpool.New(ctx, connString)
		if err != nil {
			return ctx, fmt.Errorf("open seed pool: %w", err)
		}
		st.seed = pool
		st.tracer = &statementTracer{}
		return ctx, nil
	})

	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if st.session != nil {
			_ = st.session.Close()
			st.session = nil
		}
		if st.binary != nil {
			_ = st.binary.Close()
			st.binary = nil
		}
		if st.seed != nil {
			st.seed.Close()
			st.seed = nil
		}
		if st.ro != nil {
			st.ro.Close()
			st.ro = nil
		}
		for _, d := range st.dirs {
			_ = os.RemoveAll(d)
		}
		st.dirs = nil
		st.doc = nil
		st.model = nil
		st.server = nil
		st.vars = nil
		st.tools = nil
		st.result = nil
		st.text = ""
		return ctx, nil
	})

	sc.Step(`^the SDL:$`, st.theSDL)
	sc.Step(`^I generate and apply the initial migration via goose$`, st.generateAndApply)
	sc.Step(`^the following persons exist:$`, st.personsExist)
	sc.Step(`^"([^"]*)" follows "([^"]*)"$`, st.follows)
	sc.Step(`^an MCP client connected to the server$`, st.connectClient)

	sc.Step(`^the variables:$`, st.theVariables)
	sc.Step(`^I call "([^"]*)" with:$`, st.callWith)
	sc.Step(`^I call "([^"]*)" with format "([^"]*)" and:$`, st.callWithFormat)
	sc.Step(`^I call "([^"]*)" with no arguments$`, st.callNoArgs)
	sc.Step(`^I call "([^"]*)" with type "([^"]*)"$`, st.callWithType)
	sc.Step(`^I call "([^"]*)" asking for the full schema$`, st.callFull)
	sc.Step(`^I call "([^"]*)" asking for SDL$`, st.callSDL)
	sc.Step(`^I send the introspection query a GraphQL client sends$`, st.callIntrospectionQuery)
	sc.Step(`^I connect a client to the built binary over stdio$`, st.connectBinary)
	sc.Step(`^I call "([^"]*)" on the binary with:$`, st.callBinary)

	sc.Step(`^the tool list is:$`, st.assertToolList)
	sc.Step(`^every tool declares an input schema$`, st.assertInputSchemas)
	sc.Step(`^no tool applies migrations or alters the schema$`, st.assertNoMigrationTool)
	sc.Step(`^the "([^"]*)" tool description mentions "([^"]*)"$`, st.assertDescriptionMentions)
	sc.Step(`^the "([^"]*)" tool description carries an introspection query$`, st.assertDescriptionCarriesQuery)

	sc.Step(`^the tool call succeeded$`, st.assertSucceeded)
	sc.Step(`^the tool call failed mentioning "([^"]*)"$`, st.assertFailedMentioning)
	sc.Step(`^the result JSON is:$`, st.assertJSON)
	sc.Step(`^the result JSON at "([^"]*)" is "([^"]*)"$`, st.assertJSONPath)
	sc.Step(`^the result JSON at "([^"]*)" is null$`, st.assertJSONPathNull)
	sc.Step(`^the result contains "([^"]*)"$`, st.assertContains)
	sc.Step(`^the result is the table:$`, st.assertTable)
	sc.Step(`^the result carries no SQL$`, st.assertNoSQL)
	sc.Step(`^the result carries a uuid in text form$`, st.assertTextUUID)
	sc.Step(`^the introspection result names the query root "([^"]*)"$`, st.assertQueryRoot)
	sc.Step(`^the introspection result describes the types "([^"]*)"$`, st.assertTypesDescribed)
	sc.Step(`^the type reference of "([^"]*)" is "([^"]*)"$`, st.assertTypeRef)
	sc.Step(`^the introspected type lists the fields "([^"]*)"$`, st.assertTypeFields)
	sc.Step(`^the introspected overview omits the types' field definitions$`, st.assertOverviewOmitsFields)

	sc.Step(`^no statement reached the database$`, st.assertNoStatement)
	sc.Step(`^the executed statement carries a placeholder rather than "([^"]*)"$`, st.assertBoundParameter)
	sc.Step(`^a write on the server's connection is refused by the database$`, st.assertWriteRefused)
}

// --- fixtures --------------------------------------------------------------

func (st *scenarioState) theSDL(src *godog.DocString) error {
	doc, err := sdl.Parse(src.Content)
	if err != nil {
		return err
	}
	m, err := generator.Build(doc, "")
	if err != nil {
		return err
	}
	st.doc = doc
	st.sdlSource = src.Content
	st.model = m
	return nil
}

func (st *scenarioState) generateAndApply(ctx context.Context) error {
	if st.model == nil {
		return fmt.Errorf("no schema model; the SDL step must run first")
	}
	dir, err := os.MkdirTemp("", "gopgql-mcp-migrations-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	if _, err := migrate.WriteInit(dir, st.model); err != nil {
		return err
	}

	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("open sql.DB for goose: %w", err)
	}
	defer db.Close()
	if err := goose.UpContext(ctx, db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

func (st *scenarioState) personsExist(ctx context.Context, table *godog.Table) error {
	if len(table.Rows) == 0 {
		return fmt.Errorf("persons table needs a header row")
	}
	name := -1
	for i, cell := range table.Rows[0].Cells {
		if cell.Value == "name" {
			name = i
		}
	}
	if name < 0 {
		return fmt.Errorf("persons table needs a name column")
	}
	for _, row := range table.Rows[1:] {
		if _, err := st.seed.Exec(ctx, `INSERT INTO persons (name) VALUES ($1)`, row.Cells[name].Value); err != nil {
			return fmt.Errorf("insert person: %w", err)
		}
	}
	return nil
}

func (st *scenarioState) follows(ctx context.Context, from, to string) error {
	ct, err := st.seed.Exec(ctx,
		`INSERT INTO follows (source_id, target_id)
		 SELECT s.id, t.id FROM persons s, persons t
		 WHERE s.name = $1 AND t.name = $2`, from, to)
	if err != nil {
		return fmt.Errorf("insert follows %q->%q: %w", from, to, err)
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("follows %q->%q matched %d rows, want 1", from, to, ct.RowsAffected())
	}
	return nil
}

// connectClient opens the server's read-only pool, starts the server, and
// connects a real SDK client over an in-memory transport pair.
func (st *scenarioState) connectClient(ctx context.Context) error {
	pool, err := exec.OpenReadOnly(ctx, connString, func(cfg *pgxpool.Config) {
		cfg.ConnConfig.Tracer = st.tracer
	})
	if err != nil {
		return err
	}
	st.ro = pool
	st.server = gopgqlmcp.New(st.doc, st.sdlSource, pool, gopgqlmcp.WithVersion("test"))

	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	if _, err := st.server.MCPServer().Connect(ctx, serverTransport, nil); err != nil {
		return fmt.Errorf("connect server: %w", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "gopgql-suite", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return fmt.Errorf("connect client: %w", err)
	}
	st.session = session
	return st.loadTools(ctx, session)
}

// connectBinary spawns the built binary over stdio and connects to it, so the
// wiring in cmd/gopgql-mcp is exercised rather than only the library.
func (st *scenarioState) connectBinary(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "gopgql-mcp-schema-")
	if err != nil {
		return err
	}
	st.dirs = append(st.dirs, dir)
	path := filepath.Join(dir, "schema.graphql")
	if err := os.WriteFile(path, []byte(st.sdlSource), 0o600); err != nil {
		return err
	}

	cmd := oexec.Command(binaryPath, "--sdl", path)
	// The DSN travels through the environment, the way an agent's MCP
	// configuration supplies it.
	cmd.Env = append(os.Environ(), "GOPGQL_DSN="+connString)
	cmd.Stderr = os.Stderr

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "gopgql-suite", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connect to the built binary: %w", err)
	}
	st.binary = session
	return st.loadTools(ctx, session)
}

func (st *scenarioState) loadTools(ctx context.Context, session *mcpsdk.ClientSession) error {
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	st.tools = list.Tools
	return nil
}

// --- calls -----------------------------------------------------------------

func (st *scenarioState) theVariables(src *godog.DocString) error {
	var vars map[string]any
	if err := json.Unmarshal([]byte(src.Content), &vars); err != nil {
		return fmt.Errorf("variables are not JSON: %w", err)
	}
	st.vars = vars
	return nil
}

func (st *scenarioState) call(ctx context.Context, session *mcpsdk.ClientSession, tool string, args map[string]any) error {
	if session == nil {
		return fmt.Errorf("no client session; connect one first")
	}
	st.tracer.reset()
	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return fmt.Errorf("call %s: %w", tool, err)
	}
	st.result = res
	st.text = resultText(res)
	return nil
}

func (st *scenarioState) callWith(ctx context.Context, tool string, src *godog.DocString) error {
	args := map[string]any{"query": src.Content}
	if st.vars != nil {
		args["variables"] = st.vars
	}
	return st.call(ctx, st.session, tool, args)
}

func (st *scenarioState) callWithFormat(ctx context.Context, tool, format string, src *godog.DocString) error {
	args := map[string]any{"query": src.Content, "format": format}
	if st.vars != nil {
		args["variables"] = st.vars
	}
	return st.call(ctx, st.session, tool, args)
}

func (st *scenarioState) callNoArgs(ctx context.Context, tool string) error {
	return st.call(ctx, st.session, tool, map[string]any{})
}

func (st *scenarioState) callWithType(ctx context.Context, tool, typeName string) error {
	return st.call(ctx, st.session, tool, map[string]any{"type": typeName})
}

func (st *scenarioState) callFull(ctx context.Context, tool string) error {
	return st.call(ctx, st.session, tool, map[string]any{"full": true})
}

func (st *scenarioState) callSDL(ctx context.Context, tool string) error {
	return st.call(ctx, st.session, tool, map[string]any{"format": "sdl"})
}

func (st *scenarioState) callIntrospectionQuery(ctx context.Context) error {
	return st.call(ctx, st.session, "query", map[string]any{"query": gopgqlmcp.FullIntrospectionQuery})
}

func (st *scenarioState) callBinary(ctx context.Context, tool string, src *godog.DocString) error {
	return st.call(ctx, st.binary, tool, map[string]any{"query": src.Content})
}

// resultText concatenates a tool result's text content.
func resultText(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// --- discovery assertions --------------------------------------------------

func (st *scenarioState) assertToolList(table *godog.Table) error {
	var want []string
	for _, row := range table.Rows {
		want = append(want, row.Cells[0].Value)
	}
	var got []string
	for _, tool := range st.tools {
		got = append(got, tool.Name)
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("tools = %v, want %v", got, want)
	}
	return nil
}

func (st *scenarioState) assertInputSchemas() error {
	for _, tool := range st.tools {
		if tool.Description == "" {
			return fmt.Errorf("tool %q has no description", tool.Name)
		}
		schemaMap, ok := tool.InputSchema.(map[string]any)
		if !ok {
			return fmt.Errorf("tool %q has no input schema object: %#v", tool.Name, tool.InputSchema)
		}
		if schemaMap["type"] != "object" {
			return fmt.Errorf("tool %q input schema is not an object schema: %v", tool.Name, schemaMap)
		}
		if props, ok := schemaMap["properties"].(map[string]any); !ok || len(props) == 0 {
			return fmt.Errorf("tool %q declares no properties: %v", tool.Name, schemaMap)
		}
	}
	return nil
}

// assertNoMigrationTool proves the surface has no write path: no tool named for
// migration or mutation, and nothing on the query tool that would return SQL.
func (st *scenarioState) assertNoMigrationTool() error {
	banned := []string{"migrat", "mutat", "alter", "write", "ddl", "apply"}
	for _, tool := range st.tools {
		lower := strings.ToLower(tool.Name)
		for _, word := range banned {
			if strings.Contains(lower, word) {
				return fmt.Errorf("tool %q looks like a write path", tool.Name)
			}
		}
		if tool.Name != "query" {
			continue
		}
		schemaMap, _ := tool.InputSchema.(map[string]any)
		props, _ := schemaMap["properties"].(map[string]any)
		for name := range props {
			if strings.Contains(strings.ToLower(name), "sql") {
				return fmt.Errorf("the query tool declares the argument %q, which would return SQL", name)
			}
		}
	}
	return nil
}

func (st *scenarioState) tool(name string) (*mcpsdk.Tool, error) {
	for _, tool := range st.tools {
		if tool.Name == name {
			return tool, nil
		}
	}
	return nil, fmt.Errorf("no tool named %q", name)
}

func (st *scenarioState) assertDescriptionMentions(name, substring string) error {
	tool, err := st.tool(name)
	if err != nil {
		return err
	}
	if !strings.Contains(tool.Description, substring) {
		return fmt.Errorf("the %s description does not mention %q:\n%s", name, substring, tool.Description)
	}
	return nil
}

// assertDescriptionCarriesQuery proves an agent can copy an introspection query
// out of the description and send it as-is.
func (st *scenarioState) assertDescriptionCarriesQuery(ctx context.Context, name string) error {
	tool, err := st.tool(name)
	if err != nil {
		return err
	}
	query := ""
	for _, line := range strings.Split(tool.Description, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{ __schema {") {
			query = trimmed
			break
		}
	}
	if query == "" {
		return fmt.Errorf("the %s description carries no introspection query:\n%s", name, tool.Description)
	}
	if err := st.call(ctx, st.session, "query", map[string]any{"query": query}); err != nil {
		return err
	}
	if st.result.IsError {
		return fmt.Errorf("the introspection query from the description does not run: %s", st.text)
	}
	return nil
}

// --- result assertions -----------------------------------------------------

func (st *scenarioState) assertSucceeded() error {
	if st.result == nil {
		return fmt.Errorf("no tool result")
	}
	if st.result.IsError {
		return fmt.Errorf("the tool call failed: %s", st.text)
	}
	return nil
}

func (st *scenarioState) assertFailedMentioning(substring string) error {
	if st.result == nil {
		return fmt.Errorf("no tool result")
	}
	if !st.result.IsError {
		return fmt.Errorf("the tool call succeeded; want an error mentioning %q:\n%s", substring, st.text)
	}
	if !strings.Contains(st.text, substring) {
		return fmt.Errorf("error does not mention %q:\n%s", substring, st.text)
	}
	return nil
}

func (st *scenarioState) resultJSON() (any, error) {
	if err := st.assertSucceeded(); err != nil {
		return nil, err
	}
	var data any
	if err := json.Unmarshal([]byte(st.text), &data); err != nil {
		return nil, fmt.Errorf("the result is not JSON: %w\n%s", err, st.text)
	}
	return data, nil
}

func (st *scenarioState) assertJSON(want *godog.DocString) error {
	got, err := st.resultJSON()
	if err != nil {
		return err
	}
	var wantAny any
	if err := json.Unmarshal([]byte(want.Content), &wantAny); err != nil {
		return fmt.Errorf("expected JSON is invalid: %w", err)
	}
	if !reflect.DeepEqual(canon(got), canon(wantAny)) {
		return fmt.Errorf("JSON mismatch:\n--- got ---\n%s\n--- want ---\n%s", st.text, want.Content)
	}
	return nil
}

// jsonPath walks a dotted path through the decoded result.
func (st *scenarioState) jsonPath(path string) (any, bool, error) {
	data, err := st.resultJSON()
	if err != nil {
		return nil, false, err
	}
	cur := data
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur, ok = obj[part]
		if !ok {
			return nil, false, nil
		}
	}
	return cur, true, nil
}

func (st *scenarioState) assertJSONPath(path, want string) error {
	got, ok, err := st.jsonPath(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is absent from the result:\n%s", path, st.text)
	}
	if fmt.Sprintf("%v", got) != want {
		return fmt.Errorf("%s = %v, want %q", path, got, want)
	}
	return nil
}

func (st *scenarioState) assertJSONPathNull(path string) error {
	got, ok, err := st.jsonPath(path)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is absent from the result; the specification requires an explicit null:\n%s", path, st.text)
	}
	if got != nil {
		return fmt.Errorf("%s = %v, want null", path, got)
	}
	return nil
}

func (st *scenarioState) assertContains(substring string) error {
	if err := st.assertSucceeded(); err != nil {
		return err
	}
	if !strings.Contains(st.text, substring) {
		return fmt.Errorf("the result does not contain %q:\n%s", substring, st.text)
	}
	return nil
}

func (st *scenarioState) assertTable(want *godog.DocString) error {
	if err := st.assertSucceeded(); err != nil {
		return err
	}
	got := strings.TrimRight(st.text, "\n")
	if got != strings.TrimRight(want.Content, "\n") {
		return fmt.Errorf("table mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want.Content)
	}
	return nil
}

// assertNoSQL proves the compiled statement is not part of the tool's result.
func (st *scenarioState) assertNoSQL() error {
	if err := st.assertSucceeded(); err != nil {
		return err
	}
	for _, fragment := range []string{"GRAPH_TABLE", "SELECT ", "MATCH ", "COLUMNS ("} {
		if strings.Contains(st.text, fragment) {
			return fmt.Errorf("the result leaks SQL (%q):\n%s", fragment, st.text)
		}
	}
	return nil
}

// assertTextUUID proves an id is returned as the canonical 8-4-4-4-12 text
// form rather than as the raw byte array pgx decodes a uuid into.
func (st *scenarioState) assertTextUUID() error {
	if err := st.assertSucceeded(); err != nil {
		return err
	}
	if !uuidText.MatchString(st.text) {
		return fmt.Errorf("no text-form uuid in the result:\n%s", st.text)
	}
	return nil
}

var uuidText = regexp.MustCompile(`"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"`)

// --- introspection assertions ----------------------------------------------

func (st *scenarioState) schemaResult() (map[string]any, error) {
	data, err := st.resultJSON()
	if err != nil {
		return nil, err
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the result is not an object:\n%s", st.text)
	}
	schemaObj, ok := obj["__schema"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the result carries no __schema:\n%s", st.text)
	}
	return schemaObj, nil
}

func (st *scenarioState) assertQueryRoot(want string) error {
	schemaObj, err := st.schemaResult()
	if err != nil {
		return err
	}
	queryType, ok := schemaObj["queryType"].(map[string]any)
	if !ok {
		return fmt.Errorf("no queryType in the introspection result")
	}
	if queryType["name"] != want {
		return fmt.Errorf("queryType.name = %v, want %q", queryType["name"], want)
	}
	return nil
}

// introspectedTypes indexes __schema.types by name.
func (st *scenarioState) introspectedTypes() (map[string]map[string]any, error) {
	schemaObj, err := st.schemaResult()
	if err != nil {
		return nil, err
	}
	list, ok := schemaObj["types"].([]any)
	if !ok {
		return nil, fmt.Errorf("no types in the introspection result")
	}
	out := map[string]map[string]any{}
	for _, raw := range list {
		typ, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := typ["name"].(string)
		out[name] = typ
	}
	return out, nil
}

func (st *scenarioState) assertTypesDescribed(names string) error {
	types, err := st.introspectedTypes()
	if err != nil {
		return err
	}
	for _, name := range splitList(names) {
		if _, ok := types[name]; !ok {
			return fmt.Errorf("__schema.types is missing %q", name)
		}
	}
	return nil
}

// assertTypeRef walks the ofType chain of one field, which is what a client
// that renders types generically depends on.
func (st *scenarioState) assertTypeRef(field, want string) error {
	typeName, fieldName, ok := strings.Cut(field, ".")
	if !ok {
		return fmt.Errorf("field reference %q must be Type.field", field)
	}
	types, err := st.introspectedTypes()
	if err != nil {
		return err
	}
	typ, ok := types[typeName]
	if !ok {
		return fmt.Errorf("no type %q in the introspection result", typeName)
	}
	fields, _ := typ["fields"].([]any)
	var ref map[string]any
	for _, raw := range fields {
		f, _ := raw.(map[string]any)
		if f["name"] == fieldName {
			ref, _ = f["type"].(map[string]any)
		}
	}
	if ref == nil {
		return fmt.Errorf("%s has no field %q", typeName, fieldName)
	}

	var chain []string
	for ref != nil {
		if name, _ := ref["name"].(string); name != "" {
			chain = append(chain, name)
			break
		}
		kind, _ := ref["kind"].(string)
		chain = append(chain, kind)
		ref, _ = ref["ofType"].(map[string]any)
	}
	if got := strings.Join(chain, ", "); got != strings.Join(splitList(want), ", ") {
		return fmt.Errorf("the type reference of %s is %q, want %q", field, got, want)
	}
	return nil
}

func (st *scenarioState) assertTypeFields(names string) error {
	data, err := st.resultJSON()
	if err != nil {
		return err
	}
	obj, _ := data.(map[string]any)
	typ, ok := obj["__type"].(map[string]any)
	if !ok {
		return fmt.Errorf("the result carries no __type:\n%s", st.text)
	}
	got := map[string]bool{}
	fields, _ := typ["fields"].([]any)
	for _, raw := range fields {
		f, _ := raw.(map[string]any)
		if name, _ := f["name"].(string); name != "" {
			got[name] = true
		}
	}
	for _, want := range splitList(names) {
		if !got[want] {
			return fmt.Errorf("%v has no field %q", typ["name"], want)
		}
	}
	return nil
}

// assertOverviewOmitsFields is the overview's whole point: naming every type
// without expanding any of them is what keeps the result affordable.
func (st *scenarioState) assertOverviewOmitsFields() error {
	types, err := st.introspectedTypes()
	if err != nil {
		return err
	}
	for name, typ := range types {
		if _, ok := typ["fields"]; ok {
			return fmt.Errorf("the overview expands %q; it must omit type field definitions", name)
		}
	}
	return nil
}

// --- database assertions ---------------------------------------------------

func (st *scenarioState) assertNoStatement() error {
	statements, _ := st.tracer.recorded()
	if len(statements) != 0 {
		return fmt.Errorf("%d statement(s) reached the database:\n%s", len(statements), strings.Join(statements, "\n---\n"))
	}
	return nil
}

func (st *scenarioState) assertBoundParameter(value string) error {
	statements, args := st.tracer.recorded()
	if len(statements) == 0 {
		return fmt.Errorf("no statement reached the database")
	}
	for i, stmt := range statements {
		if strings.Contains(stmt, value) {
			return fmt.Errorf("the value %q was interpolated into the statement:\n%s", value, stmt)
		}
		if !strings.Contains(stmt, "$1") {
			return fmt.Errorf("the statement carries no placeholder:\n%s", stmt)
		}
		found := false
		for _, arg := range args[i] {
			if fmt.Sprintf("%v", arg) == value {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("the value %q was not passed as a bind parameter: %v", value, args[i])
		}
	}
	return nil
}

func (st *scenarioState) assertWriteRefused(ctx context.Context) error {
	if st.ro == nil {
		return fmt.Errorf("the server's pool is not open")
	}
	_, err := st.ro.Exec(ctx, `INSERT INTO persons (name) VALUES ('intruder')`)
	if err == nil {
		return fmt.Errorf("the write succeeded; the server's connection must be read-only")
	}
	if !strings.Contains(err.Error(), "read-only") {
		return fmt.Errorf("the write failed for another reason: %w", err)
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// canon recursively canonicalizes decoded JSON so comparisons ignore array
// order (GraphQL list order for an unordered field is unspecified).
func canon(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = canon(val)
		}
		return m
	case []any:
		items := make([]any, len(t))
		for i, val := range t {
			items[i] = canon(val)
		}
		sort.Slice(items, func(i, j int) bool {
			bi, _ := json.Marshal(items[i])
			bj, _ := json.Marshal(items[j])
			return string(bi) < string(bj)
		})
		return items
	default:
		return v
	}
}
