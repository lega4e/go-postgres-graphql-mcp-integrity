package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lega4e/gopgql/sdl"
)

// recordingDB is a Querier that records the statements it is asked to run and
// answers with canned rows, so the pipeline around execution — rewriting the
// operation, binding parameters, rendering the result — can be tested without a
// container. Execution itself is covered by the integration suite.
type recordingDB struct {
	cols       []string
	values     [][]any
	statements []string
	args       [][]any
}

func (d *recordingDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.statements = append(d.statements, sql)
	d.args = append(d.args, args)
	return &cannedRows{cols: d.cols, values: d.values}, nil
}

type cannedRows struct {
	cols   []string
	values [][]any
	i      int
}

func (r *cannedRows) Close()                        {}
func (r *cannedRows) Err() error                    { return nil }
func (r *cannedRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *cannedRows) Scan(...any) error             { return nil }
func (r *cannedRows) RawValues() [][]byte           { return nil }
func (r *cannedRows) Conn() *pgx.Conn               { return nil }
func (r *cannedRows) Values() ([]any, error)        { return r.values[r.i-1], nil }
func (r *cannedRows) FieldDescriptions() []pgconn.FieldDescription {
	fds := make([]pgconn.FieldDescription, len(r.cols))
	for i, c := range r.cols {
		fds[i] = pgconn.FieldDescription{Name: c}
	}
	return fds
}

func (r *cannedRows) Next() bool {
	if r.i >= len(r.values) {
		return false
	}
	r.i++
	return true
}

func newServerWithDB(t *testing.T, db *recordingDB) *Server {
	t.Helper()
	doc, err := sdl.Parse(testSDL)
	if err != nil {
		t.Fatalf("parse SDL: %v", err)
	}
	return New(doc, testSDL, db)
}

func TestQueryBindsVariables(t *testing.T) {
	db := &recordingDB{
		cols:   []string{"v0_k", "v0_c0"},
		values: [][]any{{int64(1), "Ada"}},
	}
	s := newServerWithDB(t, db)

	out, err := s.Query(context.Background(),
		`query ByName($n: String!) { Persons(name: $n) { name } }`,
		map[string]any{"n": "Ada"}, FormatJSON)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(out, `"Ada"`) {
		t.Errorf("response is missing the row:\n%s", out)
	}
	if len(db.statements) != 1 {
		t.Fatalf("statements = %v, want exactly one", db.statements)
	}
	if strings.Contains(db.statements[0], "Ada") {
		t.Errorf("the value was interpolated into the statement:\n%s", db.statements[0])
	}
	if !strings.Contains(db.statements[0], "$1") {
		t.Errorf("the statement carries no placeholder:\n%s", db.statements[0])
	}
	if len(db.args[0]) != 1 || db.args[0][0] != "Ada" {
		t.Errorf("bind parameters = %v, want [Ada]", db.args[0])
	}
	if strings.Contains(out, "GRAPH_TABLE") || strings.Contains(out, "SELECT") {
		t.Errorf("the result must carry data alone, no SQL:\n%s", out)
	}
}

func TestQueryAnswersTypename(t *testing.T) {
	db := &recordingDB{
		cols:   []string{"v0_k", "v0_c0"},
		values: [][]any{{int64(1), "Ada"}},
	}
	s := newServerWithDB(t, db)

	out, err := s.Query(context.Background(), `{ Persons { name __typename } }`, nil, FormatJSON)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(out, `"__typename": "Person"`) {
		t.Errorf("__typename was not answered:\n%s", out)
	}
	if strings.Contains(db.statements[0], "__typename") {
		t.Errorf("__typename must not reach the compiler:\n%s", db.statements[0])
	}
}

func TestQueryTypenameAlone(t *testing.T) {
	// Stripping `__typename` empties the selection set, which the compiler
	// rejects; a surrogate key is selected instead and removed from the result.
	db := &recordingDB{cols: []string{"v0_k", "v0_c0"}, values: [][]any{{int64(1), int64(1)}}}
	s := newServerWithDB(t, db)

	out, err := s.Query(context.Background(), `{ Persons { kind: __typename } }`, nil, FormatJSON)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(out, `"kind": "Person"`) {
		t.Errorf("the aliased __typename is missing:\n%s", out)
	}
	if strings.Contains(out, keyAlias) {
		t.Errorf("the surrogate selection leaked into the result:\n%s", out)
	}
}

func TestQueryTypenameOnInterfaceIsRefused(t *testing.T) {
	db := &recordingDB{}
	s := newServerWithDB(t, db)

	_, err := s.Query(context.Background(), `{ actors { name __typename } }`, nil, FormatJSON)
	if err == nil || !strings.Contains(err.Error(), "__typename") {
		t.Fatalf("__typename on an interface position must be refused, got %v", err)
	}
	if len(db.statements) != 0 {
		t.Errorf("nothing must reach the database: %v", db.statements)
	}
}

func TestQueryErrorsReachNoDatabase(t *testing.T) {
	cases := map[string]struct {
		query   string
		vars    map[string]any
		wantSub string
	}{
		"unknown root field": {query: `{ Nope { id } }`, wantSub: "unknown root field"},
		"unknown field":      {query: `{ Persons { nope } }`, wantSub: `has no field "nope"`},
		"too deep":           {query: `{ Persons { follows { follows { follows { follows { id } } } } } }`, wantSub: "MaxDepth"},
		"missing variable":   {query: `query Q($n: String!) { Persons(name: $n) { id } }`, wantSub: "variable $n"},
		"mutation":           {query: `mutation { Persons { id } }`, wantSub: "only query operations"},
		"unparseable":        {query: `{ Persons {`, wantSub: "parse operation"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := &recordingDB{}
			s := newServerWithDB(t, db)
			_, err := s.Query(context.Background(), tc.query, tc.vars, FormatJSON)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantSub)
			}
			if len(db.statements) != 0 {
				t.Errorf("a rejected operation must send nothing to the database: %v", db.statements)
			}
		})
	}
}

func TestMarkdownFormat(t *testing.T) {
	t.Run("flat result", func(t *testing.T) {
		db := &recordingDB{
			cols:   []string{"v0_k", "v0_c0", "v0_c1"},
			values: [][]any{{int64(1), "Ada", "pipe|in name"}, {int64(2), "Linus", nil}},
		}
		s := newServerWithDB(t, db)
		out, err := s.Query(context.Background(), `{ Persons { name nickname } }`, nil, FormatMarkdown)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 4 {
			t.Fatalf("table has %d lines, want a header, a rule and two rows:\n%s", len(lines), out)
		}
		if lines[0] != "| name | nickname |" {
			t.Errorf("header = %q, want the selected fields in selection order", lines[0])
		}
		if !strings.Contains(lines[2], `pipe\|in name`) {
			t.Errorf("a pipe in a value must be escaped: %q", lines[2])
		}
		if !strings.HasSuffix(lines[3], "|  |") {
			t.Errorf("a null must render as an empty cell: %q", lines[3])
		}
	})

	t.Run("empty result is a header", func(t *testing.T) {
		db := &recordingDB{cols: []string{"v0_k", "v0_c0"}}
		s := newServerWithDB(t, db)
		out, err := s.Query(context.Background(), `{ Persons { name } }`, nil, FormatMarkdown)
		if err != nil {
			t.Fatalf("an empty result is an answer, not an error: %v", err)
		}
		if strings.TrimRight(out, "\n") != "| name |\n| --- |" {
			t.Errorf("empty table = %q", out)
		}
	})

	t.Run("nested is refused before execution", func(t *testing.T) {
		db := &recordingDB{}
		s := newServerWithDB(t, db)
		_, err := s.Query(context.Background(), `{ Persons { name follows { name } } }`, nil, FormatMarkdown)
		if err == nil {
			t.Fatal("want a refusal")
		}
		if !strings.Contains(err.Error(), "follows") || !strings.Contains(err.Error(), FormatJSON) {
			t.Errorf("the refusal must name the nesting field and point at JSON: %v", err)
		}
		if len(db.statements) != 0 {
			t.Errorf("the refusal must happen before execution: %v", db.statements)
		}
	})

	t.Run("unknown format", func(t *testing.T) {
		s := newServerWithDB(t, &recordingDB{})
		if _, err := s.Query(context.Background(), `{ Persons { name } }`, nil, "csv"); err == nil {
			t.Error("an unknown format must be rejected")
		}
	})
}

func TestIntrospectionSendsNoStatement(t *testing.T) {
	db := &recordingDB{}
	s := newServerWithDB(t, db)
	if _, err := s.Query(context.Background(), FullIntrospectionQuery, nil, FormatJSON); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(db.statements) != 0 {
		t.Errorf("introspection must be served from the schema: %v", db.statements)
	}
}
