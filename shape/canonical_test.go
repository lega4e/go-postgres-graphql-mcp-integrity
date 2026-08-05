package shape_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/compiler"
	"github.com/lega4e/gopgql/shape"
)

// The scalar contract (design D5), tested from both ends without a container.
//
// Each case gives the value as pgx would scan it out of a flat row and the value
// json.Decoder.UseNumber would produce from the database's own JSON, and
// requires that the two normalise to the same thing and encode to the same
// bytes. That is the whole of what byte-identity rests on at the leaves, and it
// is cheap enough to assert exhaustively.

// leaf builds a one-field projection over the given scalar.
func leaf(kind compiler.ScalarKind, graphQLType, columnType string) compiler.Projection {
	return compiler.Projection{Root: &compiler.Selection{
		ResponseKey: "rows", Alias: "v0", KeyColumn: "v0_k",
		Fields: []compiler.ProjectedField{{
			ResponseKey: "f", Property: "f", Column: "v0_c0",
			GraphQLType: graphQLType, ColumnType: columnType, Scalar: kind,
		}},
	}}
}

func TestScalarContractAgreesFromBothSides(t *testing.T) {
	at := time.Date(2026, 7, 30, 14, 30, 0, 0, time.FixedZone("CEST", 2*60*60))

	tests := []struct {
		name string
		kind compiler.ScalarKind
		gql  string
		col  string
		// pgxValue is the value pgx scans out of a flat row.
		pgxValue any
		// jsonValue is what the database's own JSON decodes to, with UseNumber.
		jsonValue any
		// want is the bytes both must encode that leaf to.
		want string
	}{
		{
			name: "Int", kind: compiler.ScalarInt, gql: "Int",
			pgxValue: int32(30), jsonValue: json.Number("30"), want: `30`,
		},
		{
			name: "Int as int64", kind: compiler.ScalarInt, gql: "Int",
			pgxValue: int64(-3), jsonValue: json.Number("-3"), want: `-3`,
		},
		{
			name: "Float", kind: compiler.ScalarFloat, gql: "Float",
			pgxValue: 1234.5678, jsonValue: json.Number("1234.5678"), want: `1234.5678`,
		},
		{
			// PostgreSQL writes 1e-07 and Go's %g writes 1e-07 as well, but the
			// two do not agree on the exponent threshold in general — so both
			// paths go through float64 and back out through one formatter.
			name: "Float in exponent form", kind: compiler.ScalarFloat, gql: "Float",
			pgxValue: 1e-7, jsonValue: json.Number("1e-07"), want: `1e-07`,
		},
		{
			// The trailing zero is the entire reason to ask for numeric rather
			// than double precision, and it survives on both paths.
			name: "numeric keeps its trailing zeros", kind: compiler.ScalarNumeric,
			gql: "Float", col: "numeric(10,2)",
			pgxValue: fakeNumeric("89.00"), jsonValue: json.Number("89.00"), want: `89.00`,
		},
		{
			name: "String", kind: compiler.ScalarString, gql: "String",
			pgxValue: "Alice", jsonValue: "Alice", want: `"Alice"`,
		},
		{
			name: "Boolean", kind: compiler.ScalarBoolean, gql: "Boolean",
			pgxValue: true, jsonValue: true, want: `true`,
		},
		{
			// pgx decodes uuid to a [16]byte, which would otherwise marshal as a
			// JSON array of sixteen numbers.
			name: "ID", kind: compiler.ScalarID, gql: "ID",
			pgxValue:  [16]byte{0x50, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4, 0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00},
			jsonValue: "500e8400-e29b-41d4-a716-446655440000",
			want:      `"500e8400-e29b-41d4-a716-446655440000"`,
		},
		{
			// PostgreSQL renders timestamptz in the session's TimeZone; Go
			// renders a time.Time in whatever zone it carries. Both normalise
			// to UTC, so neither depends on a setting nobody chose.
			name: "DateTime normalises to UTC", kind: compiler.ScalarDateTime, gql: "DateTime",
			pgxValue: at, jsonValue: "2026-07-30T14:30:00+02:00",
			want: `"2026-07-30T12:30:00Z"`,
		},
		{
			// The column is projected ::text under both strategies, so both
			// arrive holding the document as a string.
			name: "JSON decodes with UseNumber", kind: compiler.ScalarJSON, gql: "JSON",
			pgxValue: `{"a":19.90}`, jsonValue: `{"a":19.90}`, want: `{"a":19.90}`,
		},
		{
			name: "null", kind: compiler.ScalarString, gql: "String",
			pgxValue: nil, jsonValue: nil, want: `null`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proj := leaf(tt.kind, tt.gql, tt.col)

			fromRows, err := shape.Rows(proj, []map[string]any{{"v0_k": "k", "v0_c0": tt.pgxValue}})
			require.NoError(t, err, "the pgx-scanned value must normalise")

			doc, err := json.Marshal(map[string]any{
				"rows": []any{map[string]any{"f": tt.jsonValue}},
			})
			require.NoError(t, err)
			fromJSON, err := shape.Decode(proj, string(doc))
			require.NoError(t, err, "the JSON-decoded value must normalise")

			assert.Equal(t, fromRows, fromJSON,
				"the two paths must reach the same Go value for a %s leaf", tt.kind)

			goSide, err := shape.Encode(fromRows)
			require.NoError(t, err)
			sqlSide, err := shape.Encode(fromJSON)
			require.NoError(t, err)

			assert.Equal(t, string(goSide), string(sqlSide), "and encode to the same bytes")
			assert.Equal(t, `{"rows":[{"f":`+tt.want+`}]}`, string(goSide))
		})
	}
}

// TestNonFiniteFloatFailsOnBothPaths is task 4.6. PostgreSQL renders a NaN
// double as the JSON string "NaN" while encoding/json refuses the value
// outright, so a silent success on one side and a failure on the other is
// exactly the divergence the milestone must not have.
func TestNonFiniteFloatFailsOnBothPaths(t *testing.T) {
	proj := leaf(compiler.ScalarFloat, "Float", "")

	for _, tt := range []struct {
		name string
		pgx  any
		json string
	}{
		{"NaN", math.NaN(), `"NaN"`},
		{"positive infinity", math.Inf(1), `"Infinity"`},
		{"negative infinity", math.Inf(-1), `"-Infinity"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := shape.Rows(proj, []map[string]any{{"v0_k": "k", "v0_c0": tt.pgx}})
			var goSideErr *shape.NonFiniteFloatError
			require.ErrorAs(t, err, &goSideErr, "the Go-side path must refuse it")

			_, err = shape.Decode(proj, `{"rows":[{"f":`+tt.json+`}]}`)
			var sqlSideErr *shape.NonFiniteFloatError
			require.ErrorAs(t, err, &sqlSideErr, "and so must the SQL-side path")

			assert.Equal(t, "f", goSideErr.ResponseKey)
			assert.Equal(t, "f", sqlSideErr.ResponseKey)
		})
	}
}

// TestDecodeRejectsAKeyTheProjectionDoesNotDescribe is task 4.4a. A key the
// projection has no field for means the emitted SQL and the projection have
// diverged — a compiler bug — and it should say so rather than return a response
// quietly missing a key the Go-side path would have had.
func TestDecodeRejectsAKeyTheProjectionDoesNotDescribe(t *testing.T) {
	_, err := shape.Decode(leaf(compiler.ScalarString, "String", ""),
		`{"rows":[{"f":"Alice","stray":1}]}`)

	var unknown *shape.UnknownResponseKeyError
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, "stray", unknown.Key)
	assert.Equal(t, "rows", unknown.Level)
	assert.Contains(t, unknown.Known, "f")
}

// TestDecodeRejectsANullList is what makes the compiler's COALESCE load-bearing.
// json_agg over an empty set returns SQL NULL where the Go-side shaper returns
// an empty list; repairing that here instead of reporting it would make losing
// the COALESCE undetectable.
func TestDecodeRejectsANullList(t *testing.T) {
	_, err := shape.Decode(leaf(compiler.ScalarString, "String", ""), `{"rows":null}`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "COALESCE")
}

// TestEncodeSortsKeys records the half of byte-identity that is Go's doing:
// whatever order PostgreSQL wrote its object in, the response is encoded with
// keys sorted, identically on both paths.
func TestEncodeSortsKeys(t *testing.T) {
	got, err := shape.Encode(map[string]any{"zebra": 1, "a": 2, "bb": 3})
	require.NoError(t, err)
	assert.Equal(t, `{"a":2,"bb":3,"zebra":1}`, string(got))
}

// fakeNumeric stands in for pgtype.Numeric, which is what pgx scans a numeric
// column into. Only its json.Marshaler behaviour matters to the normaliser —
// which is why the normaliser reaches for the interface rather than the concrete
// type, keeping pgx out of the shape package's imports.
type fakeNumeric string

func (n fakeNumeric) MarshalJSON() ([]byte, error) { return []byte(n), nil }
