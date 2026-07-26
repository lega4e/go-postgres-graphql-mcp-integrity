package exec

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lega4e/gopgql/compiler"
)

// fakeRows is a pgx.Rows over canned values, so scanning can be unit-tested
// without a database. The real execution path is covered by the integration
// suites (SPEC.md §10).
type fakeRows struct {
	cols   []string
	values [][]any
	i      int
	err    error
	closed bool
}

func (r *fakeRows) Close() { r.closed = true }
func (r *fakeRows) Err() error {
	return r.err
}
func (r *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription {
	fds := make([]pgconn.FieldDescription, len(r.cols))
	for i, c := range r.cols {
		fds[i] = pgconn.FieldDescription{Name: c}
	}
	return fds
}

func (r *fakeRows) Next() bool {
	if r.i >= len(r.values) {
		return false
	}
	r.i++
	return true
}

func (r *fakeRows) Scan(...any) error { return errors.New("not used") }

func (r *fakeRows) Values() ([]any, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.values[r.i-1], nil
}

func (r *fakeRows) RawValues() [][]byte { return nil }
func (r *fakeRows) Conn() *pgx.Conn     { return nil }

// fakeDB hands out canned rows and records what it was asked to run.
type fakeDB struct {
	rows *fakeRows
	err  error

	gotSQL  string
	gotArgs []any
	calls   int
}

func (d *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	d.calls++
	d.gotSQL = sql
	d.gotArgs = args
	if d.err != nil {
		return nil, d.err
	}
	return d.rows, nil
}

func TestRowsScansByColumnName(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{
		cols:   []string{"v0_k", "v0_c0"},
		values: [][]any{{int64(1), "Ada"}, {int64(2), "Linus"}},
	}}

	got, err := Rows(context.Background(), db, "SELECT 1", "arg")
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	want := []map[string]any{
		{"v0_k": int64(1), "v0_c0": "Ada"},
		{"v0_k": int64(2), "v0_c0": "Linus"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	if db.gotSQL != "SELECT 1" || len(db.gotArgs) != 1 || db.gotArgs[0] != "arg" {
		t.Fatalf("statement = %q args = %v; the SQL and its bind parameters must be passed through", db.gotSQL, db.gotArgs)
	}
	if !db.rows.closed {
		t.Fatal("rows were not closed")
	}
}

func TestRowsEmptyResult(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{cols: []string{"v0_k"}}}
	got, err := Rows(context.Background(), db, "SELECT 1")
	if err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("rows = %v, want none", got)
	}
}

func TestRowsPropagatesErrors(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		db := &fakeDB{err: errors.New("boom")}
		if _, err := Rows(context.Background(), db, "SELECT 1"); err == nil {
			t.Fatal("want an error from a failing query")
		}
	})
	t.Run("read", func(t *testing.T) {
		db := &fakeDB{rows: &fakeRows{
			cols:   []string{"a"},
			values: [][]any{{1}},
			err:    errors.New("read failed"),
		}}
		if _, err := Rows(context.Background(), db, "SELECT 1"); err == nil {
			t.Fatal("want an error from a failing read")
		}
	})
}

func TestQueryShapesThroughTheProjection(t *testing.T) {
	// A parent selected with one nested child: two flat rows, one parent.
	proj := compiler.Projection{Root: &compiler.Selection{
		ResponseKey: "Persons",
		KeyColumn:   "v0_k",
		Fields:      []compiler.ProjectedField{{ResponseKey: "name", Property: "name", Column: "v0_c0"}},
		Children: []*compiler.Selection{{
			ResponseKey: "follows",
			KeyColumn:   "v1_k",
			Fields:      []compiler.ProjectedField{{ResponseKey: "name", Property: "name", Column: "v1_c0"}},
		}},
	}}
	db := &fakeDB{rows: &fakeRows{
		cols: []string{"v0_k", "v0_c0", "v1_k", "v1_c0"},
		values: [][]any{
			{int64(1), "Ada", int64(2), "Linus"},
			{int64(1), "Ada", int64(3), "Grace"},
		},
	}}

	got, err := Query(context.Background(), db, &compiler.Compiled{SQL: "SELECT 1", Projection: proj})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	want := map[string]any{"Persons": []any{map[string]any{
		"name": "Ada",
		"follows": []any{
			map[string]any{"name": "Linus"},
			map[string]any{"name": "Grace"},
		},
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %v, want %v", got, want)
	}
}

func TestQueryRejectsNilCompiled(t *testing.T) {
	db := &fakeDB{}
	if _, err := Query(context.Background(), db, nil); err == nil {
		t.Fatal("want an error for a nil compiled query")
	}
	if db.calls != 0 {
		t.Fatal("a nil compiled query must not reach the database")
	}
}
