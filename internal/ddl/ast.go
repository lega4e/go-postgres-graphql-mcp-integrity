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
	Default    string
	References *Reference
}

// Reference is a single-column foreign-key target: "<table> (<column>)".
type Reference struct {
	Table  string
	Column string
}

// TableConstraint is a table-level constraint such as "PRIMARY KEY (a, b)".
// Kind is the leading keyword phrase, upper-cased; Columns is its column list.
type TableConstraint struct {
	Kind    string
	Columns []string
}

// AlterTableStmt is "ALTER TABLE <name> <action>".
type AlterTableStmt struct {
	Name   string
	Action AlterAction
}

// AlterAction is the action of an ALTER TABLE: adding or dropping a column.
type AlterAction interface{ isAlterAction() }

// AddColumn is "ADD COLUMN <column>".
type AddColumn struct{ Column ColumnDef }

// DropColumn is "DROP COLUMN <name>".
type DropColumn struct{ Name string }

// CreateIndexStmt is "CREATE INDEX <name> ON <table> ( <columns> )".
type CreateIndexStmt struct {
	Name    string
	Table   string
	Columns []string
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
// PROPERTIES ( <properties> )".
type VertexTableDef struct {
	Table      string
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

func (*AddColumn) isAlterAction()  {}
func (*DropColumn) isAlterAction() {}
