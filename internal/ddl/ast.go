package ddl

// Statement is one parsed top-level DDL statement. The concrete types below are
// the whole grammar gopgql emits; a fold interpreter type-switches over them.
type Statement interface{ isStatement() }

// CreateTableStmt is "CREATE TABLE <name> ( <items> )". Items are split into the
// column definitions and any table-level constraints (gopgql emits a PRIMARY KEY
// (...) on edge tables).
type CreateTableStmt struct {
	Name        string
	Columns     []ColumnDef
	Constraints []TableConstraint
}

// ColumnDef is one column: "<name> <type>[] [PRIMARY KEY|NOT NULL] [DEFAULT
// <expr>] [REFERENCES <table> (<column>)]". Default holds the raw expression
// text (e.g. "gen_random_uuid()") verbatim.
type ColumnDef struct {
	Name       string
	Type       string
	Array      bool
	PrimaryKey bool
	NotNull    bool
	// Unique records an inline UNIQUE column constraint (SPEC.md §7 → M6).
	Unique     bool
	Default    string
	References *Reference
}

// Reference is a single-column foreign-key target: "<table> (<column>)".
type Reference struct {
	Table  string
	Column string
}

// TableConstraint is a table-level constraint such as "PRIMARY KEY (a, b)" or
// "CONSTRAINT persons_check_1 CHECK (a > b)".
//
// Name is the explicit constraint name from a leading CONSTRAINT clause, or ""
// for the anonymous form. It is preserved verbatim because a later delta drops a
// constraint *by name*: an emitter that names its constraints and a reader that
// upper-cased or discarded the name would not be two halves of one contract.
// Kind is the leading keyword phrase, upper-cased; Columns is its column list
// (PRIMARY KEY, UNIQUE) and Expr its raw body text (CHECK).
type TableConstraint struct {
	Name    string
	Kind    string
	Columns []string
	Expr    string
}

// AlterTableStmt is "ALTER TABLE <name> <action>".
type AlterTableStmt struct {
	Name   string
	Action AlterAction
}

// AlterAction is the action of an ALTER TABLE: adding or dropping a column or a
// constraint, renaming the table or one of its columns, or changing a column's
// default.
type AlterAction interface{ isAlterAction() }

// AddColumn is "ADD COLUMN <column>".
type AddColumn struct{ Column ColumnDef }

// DropColumn is "DROP COLUMN <name>".
type DropColumn struct{ Name string }

// AddConstraint is `ALTER TABLE t ADD CONSTRAINT n UNIQUE (cols…)` or
// `… CHECK (expr)`. Kind is "UNIQUE" or "CHECK"; Columns carries the column list
// of a UNIQUE (one column for @unique, several for a natural key) and Expr the
// raw text of a CHECK body, verbatim (SPEC.md §7 → M6, M7).
type AddConstraint struct {
	Name    string
	Kind    string
	Columns []string
	Expr    string
}

// DropConstraint is `ALTER TABLE t DROP CONSTRAINT n`. Only the name survives
// into the statement, which is why gopgql names every constraint it emits
// deterministically — the name is all a later delta has to go on.
type DropConstraint struct{ Name string }

// RenameTable is `ALTER TABLE t RENAME TO u`.
//
// It exists so that renaming preserves data instead of dropping and recreating.
// The reader has to understand it before the writer emits one: fold reconstructs
// prior state from gopgql's own migrations, so a rename it cannot read leaves the
// *next* delta computed against a state where the rename never happened — the
// differ sees the old name still there and emits a drop, taking the renamed
// table's rows with it (design D3).
type RenameTable struct{ NewName string }

// RenameColumn is `ALTER TABLE t RENAME COLUMN c TO d`. It carries the same
// data-loss hazard as RenameTable if the reader cannot see it.
type RenameColumn struct {
	Name    string
	NewName string
}

// SetDefault is `ALTER TABLE t ALTER COLUMN c SET DEFAULT <expr>`. Default holds
// the expression text verbatim; a changed default must never be expressed as a
// drop-and-add of the column, which would discard the column's data to change a
// property that has nothing to do with it (design D6).
type SetDefault struct {
	Column  string
	Default string
}

// DropDefault is `ALTER TABLE t ALTER COLUMN c DROP DEFAULT`.
type DropDefault struct{ Column string }

// CreateIndexStmt is "CREATE INDEX <name> ON <table> ( <columns> )".
type CreateIndexStmt struct {
	Name    string
	Table   string
	Columns []string
	// Method is the access method from `USING <method>`, or "" (SPEC.md §7 → M6).
	Method string
}

// DropIndexStmt is "DROP INDEX [IF EXISTS] <name>".
type DropIndexStmt struct {
	Name     string
	IfExists bool
}

// DropTableStmt is "DROP TABLE [IF EXISTS] <name>".
type DropTableStmt struct {
	Name     string
	IfExists bool
}

// CreatePropertyGraphStmt is "CREATE PROPERTY GRAPH <name> VERTEX TABLES (...)
// [EDGE TABLES (...)]".
type CreatePropertyGraphStmt struct {
	Name     string
	Vertices []VertexTableDef
	Edges    []EdgeTableDef
}

// VertexTableDef is one entry in VERTEX TABLES: "<table> LABEL <label>
// PROPERTIES ( <properties> )", optionally followed by further LABEL clauses.
// Label and Properties are the first clause; ExtraLabels holds the rest.
type VertexTableDef struct {
	Table      string
	Label      string
	Properties []string
	// ExtraLabels are any further "LABEL <label> PROPERTIES (...)" clauses on
	// the same table. A table carrying more than one label is how gopgql maps a
	// GraphQL interface to a label shared across tables (SPEC.md §7 → M4).
	ExtraLabels []LabelDef
}

// LabelDef is one "LABEL <label> PROPERTIES ( <properties> )" clause.
type LabelDef struct {
	Label      string
	Properties []string
}

// EdgeTableDef is one entry in EDGE TABLES, carrying both key references and the
// label/properties.
type EdgeTableDef struct {
	Table       string
	Label       string
	SourceKey   string
	SourceTable string
	SourceRef   string
	DestKey     string
	DestTable   string
	DestRef     string
	Properties  []string
}

// DropPropertyGraphStmt is "DROP PROPERTY GRAPH [IF EXISTS] <name>".
type DropPropertyGraphStmt struct {
	Name     string
	IfExists bool
}

func (*CreateTableStmt) isStatement()         {}
func (*AlterTableStmt) isStatement()          {}
func (*CreateIndexStmt) isStatement()         {}
func (*DropIndexStmt) isStatement()           {}
func (*DropTableStmt) isStatement()           {}
func (*CreatePropertyGraphStmt) isStatement() {}
func (*DropPropertyGraphStmt) isStatement()   {}

func (*AddColumn) isAlterAction()      {}
func (*DropColumn) isAlterAction()     {}
func (*AddConstraint) isAlterAction()  {}
func (*DropConstraint) isAlterAction() {}
func (*RenameTable) isAlterAction()    {}
func (*RenameColumn) isAlterAction()   {}
func (*SetDefault) isAlterAction()     {}
func (*DropDefault) isAlterAction()    {}
