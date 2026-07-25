// Package schema holds the in-memory physical model of a PostgreSQL schema
// derived from an SDL document: vertex tables, edge tables, indexes and the
// property-graph mapping.
//
// It is deliberately a set of plain data structures with no behaviour that
// contacts a database. The generator builds a Schema from the SDL model and
// renders it to DDL; from M2 the migrate package will also fold prior
// migrations into a Schema and diff two of them (SPEC.md §4.1). Keeping the
// model shared and passive is what lets both producers agree on one shape.
package schema

// Column is a single column of a vertex or edge table.
type Column struct {
	// Name is the column identifier.
	Name string
	// Type is the base PostgreSQL type, e.g. "uuid", "text", "integer".
	Type string
	// Array is true for array columns (T[]).
	Array bool
	// NotNull adds a NOT NULL constraint.
	NotNull bool
	// PrimaryKey marks the column as the table's primary key (single-column,
	// surrogate uuid in M1).
	PrimaryKey bool
	// Default is a raw default expression (e.g. "gen_random_uuid()"), or "".
	Default string
	// Unique adds a UNIQUE constraint on the column, so the database itself
	// rejects a duplicate (SPEC.md §7 → M6). PostgreSQL names the implicit
	// constraint <table>_<column>_key, which is the name a delta migration uses
	// when it adds or drops it later.
	Unique bool
	// References, when non-nil, is the foreign key target for this column.
	References *Reference
}

// Reference is a single-column foreign-key target.
type Reference struct {
	Table  string
	Column string
}

// Index is a secondary index on a table.
type Index struct {
	Name    string
	Table   string
	Columns []string
	// Method is the access method (`USING btree`, `hash`, `gin`, …) from
	// @index(using:); empty means the database default (SPEC.md §7 → M6).
	Method string
}

// UniqueConstraintName is the constraint name PostgreSQL gives an implicit
// single-column UNIQUE, and the name gopgql uses when a delta adds or drops one,
// so the two paths always agree.
func UniqueConstraintName(table, column string) string {
	return table + "_" + column + "_key"
}

// LabelProperties is one graph label exposed on a table together with the
// property list exposed under it.
//
// A table carries its own label plus, from M4, any shared labels: one label
// spanning several tables is how a GraphQL interface is mapped (SPEC.md §7 →
// M4). PostgreSQL requires every table carrying a given label to expose the
// same properties under it — same count, same names, same types — which is
// SPEC.md §5.3 invariant 5.
type LabelProperties struct {
	// Label is the graph label.
	Label string
	// Properties are the graph-exposed property names under Label.
	Properties []string
}

// VertexTable is a table mapped as a graph vertex.
type VertexTable struct {
	// Name is the physical table name.
	Name string
	// Label is the table's own graph vertex label.
	Label string
	// Columns are the table's columns in declaration order.
	Columns []Column
	// Properties are the graph-exposed property names under Label. Every KEY
	// column is re-listed here (SPEC.md §5.3 invariant 1).
	Properties []string
	// ExtraLabels are shared labels this table carries in addition to Label —
	// one per interface its GraphQL type implements — each with its own
	// property list, aligned with every other table carrying that label.
	ExtraLabels []LabelProperties
}

// EdgeTable is a table mapped as a graph edge.
type EdgeTable struct {
	// Name is the physical table name.
	Name string
	// Label is the graph edge label.
	Label string
	// Columns are the table's columns in declaration order.
	Columns []Column
	// SourceKey is the source-key column (e.g. "source_id").
	SourceKey string
	// SourceTable / SourceRef are the referenced vertex table and column.
	SourceTable string
	SourceRef   string
	// DestKey is the destination-key column (e.g. "target_id").
	DestKey string
	// DestTable / DestRef are the referenced vertex table and column.
	DestTable string
	DestRef   string
	// Properties are the graph-exposed property names, including the key
	// columns (SPEC.md §5.3 invariant 1).
	Properties []string
}

// Schema is the complete physical model: the tables, their indexes, and the
// property graph mapping them.
type Schema struct {
	// GraphName is the name of the CREATE PROPERTY GRAPH object.
	GraphName string
	// VertexTables and EdgeTables are in a stable order.
	VertexTables []VertexTable
	EdgeTables   []EdgeTable
	// Indexes are secondary indexes, including the mandatory destination-key
	// index on every edge table (SPEC.md §5.3 invariant 2).
	Indexes []Index
}
