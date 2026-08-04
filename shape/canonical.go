package shape

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lega4e/gopgql/compiler"
)

// Encode writes the canonical encoding of a GraphQL response.
//
// It is the definition byte-identity rests on (SPEC.md §7 → M8, design D3):
//
//	The response is the map[string]any returned by exec.Query. Its canonical
//	encoding is Encode, which is encoding/json over that value. Two shaping
//	strategies produce byte-identical responses when Encode of each returns
//	equal bytes.
//
// Under that definition byte-identity holds *by construction*, because the
// database's own serialisation never reaches a caller: the SQL-side path decodes
// the JSON PostgreSQL returned into the same Go value the Go-side path builds,
// and this one encoder writes both. Go's map-key sort then decides key order on
// both sides, identically.
//
// What it deliberately does **not** claim is that the bytes PostgreSQL sends
// equal the bytes gopgql writes. They do not and cannot: json_build_object emits
// `{"k" : v}` with spaces around the colon and in argument order, jsonb_build_object
// additionally sorts keys by length-then-bytes and drops duplicates, and neither
// matches encoding/json. That divergence stops at the decode boundary, which is
// the honest half of the guarantee.
func Encode(resp map[string]any) ([]byte, error) {
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("shape: encode response: %w", err)
	}
	return b, nil
}

// UnknownResponseKeyError reports a key in the database's JSON that the
// projection does not describe.
//
// It is never a silently dropped field: that case means the emitted SQL and the
// projection have diverged, which is a compiler bug, and a response quietly
// missing a key the Go-side path would have had is exactly the divergence M8
// exists to rule out. It is typed for the same reason
// *compiler.UnshapeableScalarError is — so a caller can branch on the cause
// rather than match English.
type UnknownResponseKeyError struct {
	// Level is the response key of the projection level the key appeared under.
	Level string
	// Key is the offending key.
	Key string
	// Known are the keys the projection describes at that level, in selection
	// order.
	Known []string
}

func (e *UnknownResponseKeyError) Error() string {
	return fmt.Sprintf(
		"shape: the database returned key %q at level %q, which the projection does not describe (it has %s); "+
			"the emitted SQL and the projection have diverged",
		e.Key, e.Level, strings.Join(e.Known, ", "))
}

// NonFiniteFloatError reports a Float leaf that has no JSON representation.
//
// PostgreSQL renders a NaN or infinite double precision as the JSON *string*
// "NaN" / "Infinity", while encoding/json refuses the value outright. Silently
// succeeding on one path and failing on the other is precisely the divergence
// the milestone must not have, so both paths fail here, identically (design D5).
type NonFiniteFloatError struct {
	// ResponseKey is the offending field's key in the response.
	ResponseKey string
	// Value is how the value arrived — "NaN", "Infinity", "-Infinity".
	Value string
}

func (e *NonFiniteFloatError) Error() string {
	return fmt.Sprintf(
		"shape: %s is %s, which JSON cannot represent; "+
			"PostgreSQL renders it as a string and encoding/json refuses it, so gopgql refuses it on both paths",
		e.ResponseKey, e.Value)
}

// Decode turns the JSON text an SQL-side-shaped query returns into the canonical
// response — the same map[string]any the Go-side shaper builds from flat rows.
//
// It decodes with json.Decoder.UseNumber so the database's own digits survive:
// without it `19.90` from a numeric(10,2) column becomes float64 19.9 and the
// trailing zero the database went to the trouble of keeping is lost.
func Decode(proj compiler.Projection, jsonText string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(jsonText))
	dec.UseNumber()

	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("shape: decode the database's response: %w", err)
	}

	top, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("shape: expected the database to return a JSON object, got %T", raw)
	}

	root := proj.Root
	list, err := decodeList(root, top[root.ResponseKey])
	if err != nil {
		return nil, err
	}
	return map[string]any{root.ResponseKey: list}, nil
}

// decodeList decodes one level's JSON array into the canonical list of objects.
// A missing or null array becomes an empty list, matching the Go-side shaper —
// though the compiler's COALESCE means the database should never send one.
func decodeList(sel *compiler.Selection, v any) ([]any, error) {
	if v == nil {
		return []any{}, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("shape: expected a JSON array at %q, got %T", sel.ResponseKey, v)
	}
	list := make([]any, 0, len(items))
	for _, item := range items {
		obj, err := decodeObject(sel, item)
		if err != nil {
			return nil, err
		}
		list = append(list, obj)
	}
	return list, nil
}

// decodeObject decodes one object of a level, normalising every scalar leaf and
// recursing into the nested relationships.
func decodeObject(sel *compiler.Selection, v any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("shape: expected a JSON object at %q, got %T", sel.ResponseKey, v)
	}

	obj := make(map[string]any, len(sel.Fields)+len(sel.Children))
	for _, f := range sel.Fields {
		val, err := normalise(f, m[f.ResponseKey])
		if err != nil {
			return nil, err
		}
		obj[f.ResponseKey] = val
	}
	for _, child := range sel.Children {
		list, err := decodeList(child, m[child.ResponseKey])
		if err != nil {
			return nil, err
		}
		obj[child.ResponseKey] = list
	}

	// A key the projection does not describe means the SQL and the projection
	// have diverged. Reporting it beats returning a response that is quietly
	// missing whatever else went wrong with it.
	if len(m) != len(obj) {
		for k := range m {
			if _, described := obj[k]; !described {
				return nil, &UnknownResponseKeyError{Level: sel.ResponseKey, Key: k, Known: describedKeys(sel)}
			}
		}
	}
	return obj, nil
}

// describedKeys lists the response keys a level projects, in selection order.
func describedKeys(sel *compiler.Selection) []string {
	keys := make([]string, 0, len(sel.Fields)+len(sel.Children))
	for _, f := range sel.Fields {
		keys = append(keys, f.ResponseKey)
	}
	for _, c := range sel.Children {
		keys = append(keys, c.ResponseKey)
	}
	return keys
}

// normalise maps a leaf to its canonical Go representation, reached identically
// from a pgx-scanned value and from a value decoded out of the database's own
// JSON (design D5's table). It is the whole of the scalar contract: one form per
// GraphQL scalar, so that whichever strategy produced the leaf, Encode writes
// the same bytes for it.
func normalise(f compiler.ProjectedField, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	// A list column is the element scalar's canonical form, elementwise. pgx
	// hands back a Go slice and the database's JSON hands back an array; both
	// land here.
	if f.List {
		if items, ok := asSlice(v); ok {
			out := make([]any, len(items))
			for i, item := range items {
				el, err := normalise(elementOf(f), item)
				if err != nil {
					return nil, err
				}
				out[i] = el
			}
			return out, nil
		}
	}

	switch f.Scalar {
	case compiler.ScalarInt:
		return normaliseInt(f, v)
	case compiler.ScalarFloat:
		return normaliseFloat(f, v)
	case compiler.ScalarNumeric:
		return normaliseNumeric(f, v)
	case compiler.ScalarString:
		return normaliseString(f, v)
	case compiler.ScalarBoolean:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, typeErr(f, v, "a boolean")
	case compiler.ScalarID:
		return normaliseID(f, v)
	case compiler.ScalarDateTime:
		return normaliseDateTime(f, v)
	case compiler.ScalarJSON:
		return normaliseJSON(f, v)
	default:
		// ScalarUnknown reaches here only under Go-side shaping, which makes no
		// cross-strategy promise about it: the compiler refuses it outright
		// under SQL-side shaping. Passing it through unchanged is what M3–M7
		// did, and nothing here should start guessing at it.
		return v, nil
	}
}

// elementOf is f as its list element: same scalar, one dimension down.
func elementOf(f compiler.ProjectedField) compiler.ProjectedField {
	f.List = false
	return f
}

// asSlice reports a value as a slice of elements, covering both the []any a
// JSON array decodes to and the typed slice pgx scans an array column into.
func asSlice(v any) ([]any, bool) {
	if items, ok := v.([]any); ok {
		return items, true
	}
	switch t := v.(type) {
	case []string:
		return toAny(t), true
	case []int16:
		return toAny(t), true
	case []int32:
		return toAny(t), true
	case []int64:
		return toAny(t), true
	case []float32:
		return toAny(t), true
	case []float64:
		return toAny(t), true
	case []bool:
		return toAny(t), true
	case []time.Time:
		return toAny(t), true
	default:
		return nil, false
	}
}

func toAny[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// normaliseInt canonicalises an integral leaf to a json.Number of its digits.
func normaliseInt(f compiler.ProjectedField, v any) (any, error) {
	switch t := v.(type) {
	case json.Number:
		return t, nil
	case int:
		return json.Number(strconv.FormatInt(int64(t), 10)), nil
	case int16:
		return json.Number(strconv.FormatInt(int64(t), 10)), nil
	case int32:
		return json.Number(strconv.FormatInt(int64(t), 10)), nil
	case int64:
		return json.Number(strconv.FormatInt(t, 10)), nil
	default:
		return nil, typeErr(f, v, "an integer")
	}
}

// normaliseFloat canonicalises a binary floating-point leaf.
//
// Both paths go through float64 and back out through one formatter rather than
// keeping whichever text they arrived with. PostgreSQL and Go both emit a
// shortest round-trip rendering of a double, so the two texts parse to the same
// float64 — but they do not agree on when to switch to exponent notation, and
// two spellings of one number are not byte-identical. Re-rendering settles it.
func normaliseFloat(f compiler.ProjectedField, v any) (any, error) {
	var d float64
	switch t := v.(type) {
	case json.Number:
		parsed, err := t.Float64()
		if err != nil {
			return nil, fmt.Errorf("shape: %s is not a valid Float: %w", f.ResponseKey, err)
		}
		d = parsed
	case string:
		// PostgreSQL renders a non-finite double into JSON as a string.
		return nil, &NonFiniteFloatError{ResponseKey: f.ResponseKey, Value: t}
	case float32:
		d = float64(t)
	case float64:
		d = t
	default:
		return nil, typeErr(f, v, "a float")
	}

	if math.IsNaN(d) {
		return nil, &NonFiniteFloatError{ResponseKey: f.ResponseKey, Value: "NaN"}
	}
	if math.IsInf(d, 1) {
		return nil, &NonFiniteFloatError{ResponseKey: f.ResponseKey, Value: "Infinity"}
	}
	if math.IsInf(d, -1) {
		return nil, &NonFiniteFloatError{ResponseKey: f.ResponseKey, Value: "-Infinity"}
	}
	return json.Number(strconv.FormatFloat(d, 'g', -1, 64)), nil
}

// normaliseNumeric canonicalises an exact decimal, keeping the database's own
// digits — trailing zeros included, which is the whole reason a caller asked for
// numeric rather than double precision.
//
// The SQL-side path decoded the digits straight out of the database's JSON. The
// Go-side path gets pgtype.Numeric, whose MarshalJSON emits the same digits;
// going through the json.Marshaler interface rather than the concrete type keeps
// pgx out of this package's imports.
func normaliseNumeric(f compiler.ProjectedField, v any) (any, error) {
	switch t := v.(type) {
	case json.Number:
		return t, nil
	case string:
		return nil, &NonFiniteFloatError{ResponseKey: f.ResponseKey, Value: t}
	case json.Marshaler:
		b, err := t.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("shape: %s: read numeric: %w", f.ResponseKey, err)
		}
		if bytes.Equal(b, []byte("null")) {
			return nil, nil
		}
		if len(b) > 0 && b[0] == '"' {
			var s string
			if err := json.Unmarshal(b, &s); err == nil {
				return nil, &NonFiniteFloatError{ResponseKey: f.ResponseKey, Value: s}
			}
		}
		return json.Number(b), nil
	default:
		return normaliseFloat(f, v)
	}
}

func normaliseString(f compiler.ProjectedField, v any) (any, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return nil, typeErr(f, v, "a string")
}

// normaliseID canonicalises a uuid to its 8-4-4-4-12 text form. pgx decodes a
// uuid column to a [16]byte, which would otherwise marshal as a JSON array of
// sixteen numbers; PostgreSQL's own JSON already carries the text form.
func normaliseID(f compiler.ProjectedField, v any) (any, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case [16]byte:
		return uuidString(t), nil
	default:
		return nil, typeErr(f, v, "a uuid")
	}
}

// normaliseDateTime canonicalises a timestamp to RFC3339Nano **in UTC**.
//
// PostgreSQL renders a timestamptz in the *session's* TimeZone, so the same row
// is "…+00:00" on one connection and "…+02:00" on another, and neither matches
// Go's "…Z". Converting to UTC on both paths is what stops the response
// depending on a session setting nobody set deliberately (design D5).
func normaliseDateTime(f compiler.ProjectedField, v any) (any, error) {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano), nil
	case string:
		parsed, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return nil, fmt.Errorf("shape: %s is not a valid DateTime %q: %w", f.ResponseKey, t, err)
		}
		return parsed.UTC().Format(time.RFC3339Nano), nil
	default:
		return nil, typeErr(f, v, "a timestamp")
	}
}

// normaliseJSON canonicalises an embedded JSON document to its decoded value,
// read with UseNumber so its digits survive.
//
// The column is projected ::text under both strategies, so both paths arrive
// here holding the document as a string: the Go-side path scanned that text, and
// the SQL-side path found it embedded as a JSON string. Left to the driver's own
// JSON decode instead, `19.90` inside the document would come back as float64
// 19.9 on the Go-side path only.
func normaliseJSON(f compiler.ProjectedField, v any) (any, error) {
	s, ok := v.(string)
	if !ok {
		return nil, typeErr(f, v, "a JSON document")
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("shape: %s is not a valid JSON document: %w", f.ResponseKey, err)
	}
	return out, nil
}

func typeErr(f compiler.ProjectedField, v any, want string) error {
	return fmt.Errorf("shape: %s is %s, so it should have arrived as %s, but it arrived as %T",
		f.ResponseKey, f.Scalar, want, v)
}

// uuidString formats a raw uuid in the canonical hyphenated text form.
func uuidString(u [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}
