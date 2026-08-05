package compiler

import (
	"fmt"
	"strings"
)

// Shaping selects which strategy assembles the nested GraphQL response from a
// compiled query (SPEC.md §3, decision 4; §7 → M8).
//
// It is a *compiler* option rather than an execution-time switch because the two
// strategies emit different SQL: the Go-side strategy projects one flat column
// per selected field, the SQL-side strategy projects a single `response` column
// the database has already assembled. The choice therefore has to be made
// before compilation, and nothing downstream can change it after the fact
// (design D1).
//
// The type lives in compiler rather than shape because shape already imports
// compiler, and the reverse edge would cycle.
type Shaping int

const (
	// GoSide regroups flat rows into the nested response in Go (shape.Rows).
	// It is the strategy M3 introduced and the default.
	GoSide Shaping = iota
	// SQLSide builds the nested response in-database with json_build_object and
	// json_agg, returning it as a single text column.
	SQLSide
)

// DefaultShaping is the strategy applied when none is configured. It is GoSide,
// so a caller that sets nothing gets exactly the behaviour M3–M7 shipped.
const DefaultShaping = GoSide

func (s Shaping) String() string {
	switch s {
	case GoSide:
		return "go-side"
	case SQLSide:
		return "sql-side"
	default:
		return fmt.Sprintf("Shaping(%d)", int(s))
	}
}

// WithShaping selects the result-shaping strategy (DefaultShaping by default),
// mirroring WithMaxDepth. A Compiler holds no mutable state, so a caller that
// wants both strategies over one SDL document constructs two Compilers.
func WithShaping(s Shaping) Option {
	return func(c *Compiler) { c.shaping = s }
}

// Shaping reports the configured result-shaping strategy.
func (c *Compiler) Shaping() Shaping { return c.shaping }

// ScalarKind classifies a projected scalar by the canonical response form its
// value takes, which is what lets the Go-side and SQL-side strategies agree on a
// leaf (design D5).
//
// The classification is derived from the GraphQL type the SDL declares and from
// any @column(type:) override, because the override is what decides the physical
// type the value is read out of — a Float stored as `numeric` and a Float stored
// as `double precision` reach Go as different things and render into JSON
// differently.
type ScalarKind int

const (
	// ScalarUnknown is a scalar gopgql has no canonical form for. It is
	// accepted under GoSide, which makes no cross-strategy promise about it,
	// and refused at compile time under SQLSide.
	ScalarUnknown ScalarKind = iota
	// ScalarInt is an integral value; canonically a json.Number of its digits.
	ScalarInt
	// ScalarFloat is a binary floating-point value; canonically a json.Number.
	// A non-finite value has no canonical form and is an error on both paths.
	ScalarFloat
	// ScalarNumeric is an exact decimal; canonically a json.Number carrying the
	// database's own digits, trailing zeros included.
	ScalarNumeric
	// ScalarString is text; canonically a Go string.
	ScalarString
	// ScalarBoolean is a boolean; canonically a Go bool.
	ScalarBoolean
	// ScalarID is a uuid; canonically its 8-4-4-4-12 text form.
	ScalarID
	// ScalarDateTime is a timestamp with time zone; canonically RFC3339Nano in
	// UTC, so the response does not depend on the session TimeZone.
	ScalarDateTime
	// ScalarJSON is an embedded JSON document; canonically the value decoded
	// with json.Decoder.UseNumber so its digits survive.
	ScalarJSON
)

func (k ScalarKind) String() string {
	switch k {
	case ScalarInt:
		return "Int"
	case ScalarFloat:
		return "Float"
	case ScalarNumeric:
		return "Numeric"
	case ScalarString:
		return "String"
	case ScalarBoolean:
		return "Boolean"
	case ScalarID:
		return "ID"
	case ScalarDateTime:
		return "DateTime"
	case ScalarJSON:
		return "JSON"
	default:
		return "unknown"
	}
}

// UnshapeableScalarError reports a projected field whose scalar has no canonical
// response form, so the two shaping strategies could not be held to producing
// the same value for it.
//
// It is returned at compile time and **only under SQLSide** — GoSide keeps
// accepting the field, because it makes no cross-strategy promise about it.
// Refusing to compile is the only outcome that cannot quietly break the
// byte-identity guarantee (design D5).
//
// It is a typed error, alongside *DepthExceededError, so a caller that wants to
// fall back to Go-side shaping can branch on the cause rather than match
// English.
type UnshapeableScalarError struct {
	// TypeName is the GraphQL type declaring the field.
	TypeName string
	// Field is the field's GraphQL name.
	Field string
	// GraphQLType is the field's declared GraphQL scalar (Int, DateTime, …).
	GraphQLType string
	// ColumnType is the @column(type:) override, or empty when the field uses
	// the default scalar mapping (SPEC.md §5.1).
	ColumnType string
}

func (e *UnshapeableScalarError) Error() string {
	via := "the default scalar mapping"
	if e.ColumnType != "" {
		via = fmt.Sprintf("@column(type: %q)", e.ColumnType)
	}
	return fmt.Sprintf(
		"compiler: %s.%s is %s via %s, which has no canonical response form, "+
			"so SQL-side shaping cannot promise the same value as Go-side shaping; "+
			"compile this query with compiler.GoSide, or map the field to a supported type",
		e.TypeName, e.Field, e.GraphQLType, via)
}

// classifyScalar derives a projected field's ScalarKind from the GraphQL type
// the SDL declares and the @column(type:) override, if any. The override wins:
// it is what decides the physical type, and so what decides how the value
// renders on each path.
func classifyScalar(graphQLType, columnType string) ScalarKind {
	if columnType != "" {
		return classifyColumnType(columnType)
	}
	// SPEC.md §5.1's default mapping. A type absent from it is a custom scalar
	// or an enum, which gopgql has no canonical form for.
	switch graphQLType {
	case "Int":
		return ScalarInt
	case "Float":
		return ScalarFloat
	case "String":
		return ScalarString
	case "Boolean":
		return ScalarBoolean
	case "ID":
		return ScalarID
	case "DateTime":
		return ScalarDateTime
	case "JSON":
		return ScalarJSON
	default:
		return ScalarUnknown
	}
}

// classifyColumnType maps a PostgreSQL type written in @column(type:) to its
// canonical response form. The base name is taken before any modifier, so
// `numeric(10,2)` and `varchar(64)` classify as `numeric` and `varchar`.
//
// A type absent from this list — hstore, interval, a domain, an enum — is
// deliberately ScalarUnknown rather than guessed at: a wrong guess is exactly
// the silent divergence design D5 exists to prevent.
func classifyColumnType(columnType string) ScalarKind {
	base := strings.ToLower(strings.TrimSpace(columnType))
	if i := strings.IndexByte(base, '('); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	base = strings.Join(strings.Fields(base), " ")

	switch base {
	case "smallint", "integer", "int", "int2", "int4", "bigint", "int8":
		return ScalarInt
	case "real", "double precision", "float4", "float8":
		return ScalarFloat
	case "numeric", "decimal":
		return ScalarNumeric
	case "text", "varchar", "character varying", "char", "character", "citext", "name":
		return ScalarString
	case "boolean", "bool":
		return ScalarBoolean
	case "uuid":
		return ScalarID
	case "timestamptz", "timestamp with time zone":
		return ScalarDateTime
	case "json", "jsonb":
		return ScalarJSON
	default:
		// `timestamp` without a time zone lands here on purpose: it renders
		// with no offset at all, so its text form cannot be reconciled with a
		// timestamptz's, and pretending otherwise would be the guess D5 forbids.
		return ScalarUnknown
	}
}
