package ddl

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseOne parses src, requiring exactly one statement, and returns it.
func parseOne(t *testing.T, src string) Statement {
	t.Helper()
	stmts, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	if len(stmts) != 1 {
		t.Fatalf("Parse(%q): got %d statements, want 1", src, len(stmts))
	}
	return stmts[0]
}

func TestLexQuotedIdentEscapes(t *testing.T) {
	toks, err := Lex(`"a""b" bare , ()[]`)
	if err != nil {
		t.Fatalf("Lex: %v", err)
	}
	want := []struct {
		tt  TokenType
		val string
	}{
		{TokenQuotedIdent, `a"b`},
		{TokenWord, "bare"},
		{TokenComma, ","},
		{TokenLParen, "("},
		{TokenRParen, ")"},
		{TokenLBracket, "["},
		{TokenRBracket, "]"},
		{TokenEOF, ""},
	}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens, want %d: %+v", len(toks), len(want), toks)
	}
	for i, w := range want {
		if toks[i].Type != w.tt || toks[i].Value != w.val {
			t.Errorf("token %d = {%v %q}, want {%v %q}", i, toks[i].Type, toks[i].Value, w.tt, w.val)
		}
	}
}

func TestLexUnterminatedQuote(t *testing.T) {
	if _, err := Lex(`"oops`); err == nil {
		t.Fatal("expected error for unterminated quoted identifier")
	}
	if _, err := Lex(`'oops`); err == nil {
		t.Fatal("expected error for unterminated string")
	}
}

func TestParseCreateTableColumns(t *testing.T) {
	src := `CREATE TABLE persons (
	    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	    name text NOT NULL,
	    email text,
	    ratio double precision,
	    tags text[]
	)`
	st, ok := parseOne(t, src).(*CreateTableStmt)
	if !ok {
		t.Fatalf("want *CreateTableStmt, got %T", parseOne(t, src))
	}
	if st.Name != "persons" {
		t.Errorf("Name = %q, want persons", st.Name)
	}
	want := []ColumnDef{
		{Name: "id", Type: "uuid", PrimaryKey: true, Default: "gen_random_uuid()"},
		{Name: "name", Type: "text", NotNull: true},
		{Name: "email", Type: "text"},
		{Name: "ratio", Type: "double precision"},
		{Name: "tags", Type: "text", Array: true},
	}
	if !reflect.DeepEqual(st.Columns, want) {
		t.Errorf("columns mismatch\ngot  %+v\nwant %+v", st.Columns, want)
	}
}

func TestParseColumnReference(t *testing.T) {
	st := parseOne(t, `CREATE TABLE follows (
	    source_id uuid NOT NULL REFERENCES persons (id),
	    target_id uuid NOT NULL REFERENCES persons (id),
	    PRIMARY KEY (source_id, target_id)
	)`).(*CreateTableStmt)

	if len(st.Columns) != 2 {
		t.Fatalf("got %d columns, want 2", len(st.Columns))
	}
	ref := st.Columns[0].References
	if ref == nil || ref.Table != "persons" || ref.Column != "id" {
		t.Errorf("source_id reference = %+v, want persons(id)", ref)
	}
	if len(st.Constraints) != 1 {
		t.Fatalf("got %d table constraints, want 1", len(st.Constraints))
	}
	pk := st.Constraints[0]
	if pk.Kind != "PRIMARY KEY" || !reflect.DeepEqual(pk.Columns, []string{"source_id", "target_id"}) {
		t.Errorf("constraint = %+v, want PRIMARY KEY (source_id, target_id)", pk)
	}
}

func TestParseQuotedKeywordIdentifiers(t *testing.T) {
	// "order" is a reserved word the generator double-quotes; it must fold back
	// to the bare identifier order, and never be mistaken for a keyword.
	st := parseOne(t, `CREATE TABLE "order" (
	    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	    total double precision NOT NULL
	)`).(*CreateTableStmt)
	if st.Name != "order" {
		t.Errorf("Name = %q, want order", st.Name)
	}
	if st.Columns[1].Type != "double precision" {
		t.Errorf("total type = %q, want double precision", st.Columns[1].Type)
	}
}

// TestParseTypeModifier proves the grammar extends to a parenthesised type
// modifier (M6's @column(type:)) without any change to the interpreter shape.
func TestParseTypeModifier(t *testing.T) {
	st := parseOne(t, `CREATE TABLE t ( price numeric(10, 2) NOT NULL )`).(*CreateTableStmt)
	got := st.Columns[0]
	if got.Type != "numeric(10, 2)" || !got.NotNull {
		t.Errorf("column = %+v, want numeric(10, 2) NOT NULL", got)
	}
}

func TestParseAlterTable(t *testing.T) {
	add := parseOne(t, `ALTER TABLE persons ADD COLUMN age integer`).(*AlterTableStmt)
	if add.Name != "persons" {
		t.Errorf("Name = %q, want persons", add.Name)
	}
	a, ok := add.Action.(*AddColumn)
	if !ok {
		t.Fatalf("action = %T, want *AddColumn", add.Action)
	}
	if a.Column.Name != "age" || a.Column.Type != "integer" {
		t.Errorf("added column = %+v, want age integer", a.Column)
	}

	drop := parseOne(t, `ALTER TABLE persons DROP COLUMN age`).(*AlterTableStmt)
	d, ok := drop.Action.(*DropColumn)
	if !ok || d.Name != "age" {
		t.Errorf("action = %+v, want DropColumn{age}", drop.Action)
	}
}

func TestParseIndexAndDrops(t *testing.T) {
	idx := parseOne(t, `CREATE INDEX follows_target_idx ON follows (target_id)`).(*CreateIndexStmt)
	if idx.Name != "follows_target_idx" || idx.Table != "follows" ||
		!reflect.DeepEqual(idx.Columns, []string{"target_id"}) {
		t.Errorf("index = %+v", idx)
	}

	di := parseOne(t, `DROP INDEX IF EXISTS follows_target_idx`).(*DropIndexStmt)
	if di.Name != "follows_target_idx" || !di.IfExists {
		t.Errorf("drop index = %+v", di)
	}

	dt := parseOne(t, `DROP TABLE IF EXISTS follows`).(*DropTableStmt)
	if dt.Name != "follows" || !dt.IfExists {
		t.Errorf("drop table = %+v", dt)
	}

	dg := parseOne(t, `DROP PROPERTY GRAPH app_graph`).(*DropPropertyGraphStmt)
	if dg.Name != "app_graph" || dg.IfExists {
		t.Errorf("drop graph = %+v", dg)
	}
}

func TestParsePropertyGraph(t *testing.T) {
	src := `CREATE PROPERTY GRAPH app_graph
	  VERTEX TABLES (
	    persons LABEL person PROPERTIES (id, name, email)
	  )
	  EDGE TABLES (
	    follows SOURCE KEY (source_id) REFERENCES persons (id)
	            DESTINATION KEY (target_id) REFERENCES persons (id)
	            LABEL follows PROPERTIES (source_id, target_id)
	  )`
	g := parseOne(t, src).(*CreatePropertyGraphStmt)
	if g.Name != "app_graph" {
		t.Errorf("Name = %q, want app_graph", g.Name)
	}
	wantV := []VertexTableDef{{Table: "persons", Label: "person", Properties: []string{"id", "name", "email"}}}
	if !reflect.DeepEqual(g.Vertices, wantV) {
		t.Errorf("vertices = %+v, want %+v", g.Vertices, wantV)
	}
	wantE := []EdgeTableDef{{
		Table: "follows", Label: "follows",
		SourceKey: []string{"source_id"}, SourceTable: "persons", SourceRef: []string{"id"},
		DestKey: []string{"target_id"}, DestTable: "persons", DestRef: []string{"id"},
		Properties: []string{"source_id", "target_id"},
	}}
	if !reflect.DeepEqual(g.Edges, wantE) {
		t.Errorf("edges = %+v, want %+v", g.Edges, wantE)
	}
}

func TestParsePropertyGraphVertexOnly(t *testing.T) {
	g := parseOne(t, `CREATE PROPERTY GRAPH g VERTEX TABLES ( t LABEL l PROPERTIES (id) )`).(*CreatePropertyGraphStmt)
	if len(g.Vertices) != 1 || len(g.Edges) != 0 {
		t.Errorf("want 1 vertex 0 edges, got %d/%d", len(g.Vertices), len(g.Edges))
	}
}

func TestParseMultipleStatements(t *testing.T) {
	src := `CREATE TABLE a ( id uuid PRIMARY KEY DEFAULT gen_random_uuid() );
	CREATE INDEX a_idx ON a (id);
	DROP TABLE IF EXISTS b;`
	stmts, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(stmts))
	}
	if _, ok := stmts[0].(*CreateTableStmt); !ok {
		t.Errorf("stmt 0 = %T, want *CreateTableStmt", stmts[0])
	}
	if _, ok := stmts[1].(*CreateIndexStmt); !ok {
		t.Errorf("stmt 1 = %T, want *CreateIndexStmt", stmts[1])
	}
	if _, ok := stmts[2].(*DropTableStmt); !ok {
		t.Errorf("stmt 2 = %T, want *DropTableStmt", stmts[2])
	}
}

func TestParseEmpty(t *testing.T) {
	for _, src := range []string{"", "   ", ";", ";;\n;"} {
		stmts, err := Parse(src)
		if err != nil {
			t.Errorf("Parse(%q): %v", src, err)
		}
		if len(stmts) != 0 {
			t.Errorf("Parse(%q): got %d statements, want 0", src, len(stmts))
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		`SELECT 1`,                                       // not a DDL statement gopgql emits
		`CREATE VIEW v AS SELECT 1`,                      // unrecognised CREATE target
		`CREATE TABLE t ( id uuid`,                       // missing ')'
		`CREATE TABLE t id uuid )`,                       // missing '('
		`ALTER TABLE t RENAME`,                           // RENAME with no target
		`ALTER TABLE t ALTER COLUMN c SET NOT NULL`,      // only the default is alterable
		`ALTER TABLE t ADD CONSTRAINT c FOREIGN KEY (a)`, // gopgql emits UNIQUE and CHECK only
		`ALTER TABLE t ADD CONSTRAINT c CHECK ()`,        // an empty check body
		`ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0`,    // unterminated check body
		`CREATE INDEX i persons (id)`,                    // missing ON
		`CREATE TABLE t ( id uuid ) EXTRA`,               // trailing tokens
		`CREATE PROPERTY GRAPH g VERTEX ( t )`,           // VERTEX not followed by TABLES
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q): expected error, got none", src)
		}
	}
}

// TestParseErrorMentionsOffset checks errors are locatable.
func TestParseErrorMentionsOffset(t *testing.T) {
	_, err := Parse(`CREATE TABLE t ( id uuid`)
	if err == nil || !strings.Contains(err.Error(), "offset") {
		t.Fatalf("expected an error mentioning an offset, got %v", err)
	}
}

// TestParsePropertyGraphMultipleLabels covers a vertex table carrying several
// LABEL clauses — how gopgql maps a GraphQL interface to a label shared across
// tables (SPEC.md §7 → M4). Fold must read back everything the generator writes.
func TestParsePropertyGraphMultipleLabels(t *testing.T) {
	src := `CREATE PROPERTY GRAPH app_graph
	  VERTEX TABLES (
	    bots LABEL bot PROPERTIES (id, name, vendor)
	         LABEL actor PROPERTIES (id, name),
	    persons LABEL person PROPERTIES (id, name, email)
	            LABEL actor PROPERTIES (id, name)
	  )`
	g := parseOne(t, src).(*CreatePropertyGraphStmt)
	want := []VertexTableDef{
		{
			Table: "bots", Label: "bot", Properties: []string{"id", "name", "vendor"},
			ExtraLabels: []LabelDef{{Label: "actor", Properties: []string{"id", "name"}}},
		},
		{
			Table: "persons", Label: "person", Properties: []string{"id", "name", "email"},
			ExtraLabels: []LabelDef{{Label: "actor", Properties: []string{"id", "name"}}},
		},
	}
	if !reflect.DeepEqual(g.Vertices, want) {
		t.Errorf("vertices = %+v, want %+v", g.Vertices, want)
	}
}

// alterAction parses src as a single ALTER TABLE on the named table and returns
// its action.
func alterAction(t *testing.T, src, table string) AlterAction {
	t.Helper()
	st, ok := parseOne(t, src).(*AlterTableStmt)
	require.True(t, ok, "%s must parse as an ALTER TABLE", src)
	require.Equal(t, table, st.Name)
	return st.Action
}

// TestParseAlterTableRename covers the two rename forms. They are read before
// anything emits one on purpose (design D3): a rename the fold cannot read
// leaves the next delta computed against a state where the rename never
// happened, so the differ emits a drop and the renamed data goes with it.
func TestParseAlterTableRename(t *testing.T) {
	assert.Equal(t, &RenameTable{NewName: "people"},
		alterAction(t, `ALTER TABLE persons RENAME TO people`, "persons"))

	assert.Equal(t, &RenameColumn{Name: "email", NewName: "contact"},
		alterAction(t, `ALTER TABLE persons RENAME COLUMN email TO contact`, "persons"))

	// A reserved word is double-quoted by the emitter on both sides of the
	// rename and must come back as the bare identifier.
	assert.Equal(t, &RenameTable{NewName: "order"},
		alterAction(t, `ALTER TABLE "user" RENAME TO "order"`, "user"))
}

// TestParseAlterTableConstraints covers ADD/DROP CONSTRAINT for both bodies the
// emitter produces: the UNIQUE of @unique and of a natural key, and a CHECK.
func TestParseAlterTableConstraints(t *testing.T) {
	assert.Equal(t, &AddConstraint{Name: "products_sku_key", Kind: "UNIQUE", Columns: []string{"sku"}},
		alterAction(t, `ALTER TABLE products ADD CONSTRAINT products_sku_key UNIQUE (sku)`, "products"),
		"a single-column UNIQUE is what @unique maps to")

	assert.Equal(t, &AddConstraint{Name: "products_key", Kind: "UNIQUE", Columns: []string{"tenant", "sku"}},
		alterAction(t, `ALTER TABLE products ADD CONSTRAINT products_key UNIQUE (tenant, sku)`, "products"),
		"a natural key's columns keep their declared order")

	assert.Equal(t, &AddConstraint{Name: "products_price_check", Kind: "CHECK", Expr: "price > 0"},
		alterAction(t, `ALTER TABLE products ADD CONSTRAINT products_price_check CHECK (price > 0)`, "products"))

	assert.Equal(t, &DropConstraint{Name: "products_price_check"},
		alterAction(t, `ALTER TABLE products DROP CONSTRAINT products_price_check`, "products"))
}

// TestParseCheckExprVerbatim proves a check body survives parsing unchanged.
// gopgql does not parse the expression — PostgreSQL is the authority on whether
// it is valid (design, Non-Goals) — so the reader has to hand back exactly what
// the writer wrote, nested parentheses, operators, literals and all.
func TestParseCheckExprVerbatim(t *testing.T) {
	for _, expr := range []string{
		`price > 0`,
		`(price > 0) AND (discount < price)`,
		`status IN ('open', 'closed')`,
		`char_length(sku) > 2`,
	} {
		got := alterAction(t, `ALTER TABLE products ADD CONSTRAINT c CHECK (`+expr+`)`, "products")
		add, ok := got.(*AddConstraint)
		require.True(t, ok)
		assert.Equal(t, expr, add.Expr)
	}
}

// TestParseAlterColumnDefault covers SET/DROP DEFAULT — the statements that let
// a default change without the column being dropped and re-added under it.
func TestParseAlterColumnDefault(t *testing.T) {
	assert.Equal(t, &SetDefault{Column: "active", Default: "true"},
		alterAction(t, `ALTER TABLE persons ALTER COLUMN active SET DEFAULT true`, "persons"))

	assert.Equal(t, &SetDefault{Column: "created_at", Default: "now()"},
		alterAction(t, `ALTER TABLE persons ALTER COLUMN created_at SET DEFAULT now()`, "persons"))

	assert.Equal(t, &SetDefault{Column: "kind", Default: "'person'"},
		alterAction(t, `ALTER TABLE persons ALTER COLUMN kind SET DEFAULT 'person'`, "persons"),
		"a quoted default keeps its quotes; it is emitted into DDL verbatim")

	assert.Equal(t, &DropDefault{Column: "active"},
		alterAction(t, `ALTER TABLE persons ALTER COLUMN active DROP DEFAULT`, "persons"))
}

// TestParseNamedTableConstraints covers the named constraint forms inside a
// CREATE TABLE. The name matters as much as the body: a later delta drops a
// constraint by the name the emitter chose, so a reader that upper-cased or
// discarded it would break the drop path rather than the create path — the
// harder failure to see.
func TestParseNamedTableConstraints(t *testing.T) {
	st, ok := parseOne(t, `CREATE TABLE products (
	    tenant text NOT NULL,
	    sku text NOT NULL,
	    price double precision,
	    CONSTRAINT products_key UNIQUE (tenant, sku),
	    CONSTRAINT products_check_1 CHECK (price > 0)
	)`).(*CreateTableStmt)
	require.True(t, ok)
	assert.Equal(t, []TableConstraint{
		{Name: "products_key", Kind: "UNIQUE", Columns: []string{"tenant", "sku"}},
		{Name: "products_check_1", Kind: "CHECK", Expr: "price > 0"},
	}, st.Constraints)
}
