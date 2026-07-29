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

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

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
	// Check is the body of a column-level CHECK constraint, or "". It is raw SQL
	// the generator never parses (design D6): PostgreSQL rejects a bad expression
	// when the migration is applied, which is the right place and the right
	// error. The constraint is emitted named — ColumnCheckConstraintName — so a
	// later delta can drop it without asking the database what name it invented.
	Check string
	// References, when non-nil, is the foreign key target for this column.
	References *Reference
	// RenamedFrom holds the physical column names this column may previously
	// have had, most likely first, derived from an @renamedFrom hint on the
	// field (design D2).
	//
	// It is a *candidate list* rather than a single name because @renamedFrom
	// states the prior GraphQL name, and the mapping from a GraphQL name to the
	// column it produced depends on a @column(name:) override that only the
	// prior SDL knew. The differ resolves the ambiguity the only way it can be
	// resolved: against the folded prior state, taking the first candidate that
	// is actually there. A candidate that matches nothing is a no-op, never an
	// error, because the hint outlives the rename it describes.
	//
	// Nothing in the DDL carries it, so a folded schema never has one: it is an
	// instruction from the SDL to the differ, not a property of the database.
	RenamedFrom []string
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

// NaturalKey is a named uniqueness constraint over a vertex table's own scalar
// columns: the @key(fields:) natural key.
//
// It sits *alongside* the surrogate id, which stays the physical identity that
// edge tables reference (design D1). The columns are in the order the SDL
// declared them, because a multi-column UNIQUE is an ordered constraint and the
// same order is what the property graph's KEY clause lists.
type NaturalKey struct {
	// Name is the constraint name, always NaturalKeyConstraintName(table).
	Name string
	// Columns are the constrained columns, in declaration order.
	Columns []string
}

// The constraint names below are the whole reason gopgql spells its constraints
// out instead of letting PostgreSQL invent them: a delta drops a constraint *by
// name*, and the only name it can use without asking a live database is one both
// halves derive from the same rule. Fold classifies a bare DROP CONSTRAINT by
// running these rules backwards (ClassifyConstraint), so emitter and reader
// cannot drift apart.

// UniqueConstraintName is the constraint name PostgreSQL gives an implicit
// single-column UNIQUE, and the name gopgql uses when a delta adds or drops one,
// so the two paths always agree.
func UniqueConstraintName(table, column string) string {
	return table + "_" + column + "_key"
}

// NaturalKeyConstraintName is the name of a table's natural-key UNIQUE
// constraint. It deliberately omits any column, so the name survives a change to
// the key's column list: the delta drops one constraint and adds one back.
func NaturalKeyConstraintName(table string) string {
	return table + "_key"
}

// ColumnCheckConstraintName names a column-level CHECK, matching the name
// PostgreSQL would itself derive for one.
func ColumnCheckConstraintName(table, column string) string {
	return table + "_" + column + "_check"
}

// TableCheckConstraintName names the n-th (1-based) table-level CHECK. A
// table-level check spans columns, so it has no column to be named after; the
// ordinal is its declaration position in the SDL.
func TableCheckConstraintName(table string, n int) string {
	return fmt.Sprintf("%s_check_%d", table, n)
}

// ConstraintRole is what a constraint name means under the rules above.
type ConstraintRole int

// The constraint roles gopgql emits. RoleUnknown covers everything else,
// including a constraint left behind under a stale name by a rename.
const (
	RoleUnknown ConstraintRole = iota
	RoleNaturalKey
	RoleColumnUnique
	RoleColumnCheck
	RoleTableCheck
)

// tableCheckRe matches the ordinal of a table-level check name once the table
// prefix has been stripped: "check_3" → 3.
var tableCheckRe = regexp.MustCompile(`^check_(\d+)$`)

// ClassifyConstraint reads a constraint name back into the role that produced
// it. column is set for the column-scoped roles and ordinal for RoleTableCheck.
//
// It is the inverse of the four naming functions above, and it exists because a
// DROP CONSTRAINT statement carries nothing but a name: folding one back onto
// the model means recovering from the name what the constraint was. The order of
// the tests matters — "<table>_key" is a natural key, not a UNIQUE on a column
// called "" — so it is written once, here, rather than in each caller.
func ClassifyConstraint(table, name string) (role ConstraintRole, column string, ordinal int) {
	if name == NaturalKeyConstraintName(table) {
		return RoleNaturalKey, "", 0
	}
	rest, ok := strings.CutPrefix(name, table+"_")
	if !ok {
		return RoleUnknown, "", 0
	}
	if m := tableCheckRe.FindStringSubmatch(rest); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return RoleUnknown, "", 0
		}
		return RoleTableCheck, "", n
	}
	if col, ok := strings.CutSuffix(rest, "_check"); ok && col != "" {
		return RoleColumnCheck, col, 0
	}
	if col, ok := strings.CutSuffix(rest, "_key"); ok && col != "" {
		return RoleColumnUnique, col, 0
	}
	return RoleUnknown, "", 0
}

// NormalizeExpr collapses runs of whitespace in a raw SQL fragment — a @check
// expression or a @default value — to single spaces.
//
// The generator normalizes on the way in so that the text it emits is the text
// internal/ddl reads back out, which normalizes the same way. Without it a
// schema whose check is written `age  >= 0` would fold back as `age >= 0`, the
// differ would call that a changed constraint, and every run would emit a
// migration that changes nothing.
func NormalizeExpr(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
	// Checks are table-level CHECK bodies — the form that spans more than one
	// column — in declaration order. Raw SQL, like Column.Check; the ordinal in
	// each constraint's name is its position here (design D6).
	Checks []string
	// NaturalKey is the @key(fields:) constraint, or nil. Its columns are also
	// listed in the property graph's KEY clause, so a MATCH can select a vertex
	// by them (design D1).
	NaturalKey *NaturalKey
	// RenamedFrom holds the physical table names this table may previously have
	// had, most likely first. See Column.RenamedFrom for why it is a candidate
	// list and why a folded schema never has one.
	RenamedFrom []string
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
