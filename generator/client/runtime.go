package client

// runtimeSource is the fixed part of every generated client: the Client itself
// and the handful of decoders the generated assignments call.
//
// It is emitted into the generated package rather than imported from gopgql, so
// that the generated code depends on gopgql for the compiler and exec types it
// genuinely needs and for nothing else. These are a handful of small functions;
// a package to hold them would be a package to version.
//
// The decoders widen rather than assert, and they are widened against **two**
// producers, which is the part that was got wrong once and is worth stating
// plainly (issue #51).
//
// A query's value has been through shape: every leaf is canonicalised there, so
// an Int, a Float and a numeric all arrive as json.Number and a DateTime arrives
// as RFC3339Nano text. That is deliberate — it is what makes the Go-side and
// SQL-side strategies produce byte-identical responses — and both strategies
// reach it, so a decoder that cannot read the canonical form is broken on both
// paths at once. A mutation's value has been through nothing at all: exec.Call
// hands back whatever pgx scanned, so int32, float64, time.Time and
// pgtype.Numeric arrive as themselves. Each decoder therefore accepts the
// canonical form *and* the driver's, because `integer` arriving as int32 where
// the field is int64 is a difference in the driver, not in the schema.
//
// What is *not* widened is a mismatch that means something: a null in a non-null
// field, or a value of a kind the field cannot hold — 41.5 for an Int, a
// PostgreSQL NaN for a Float — both stop with the field's path in the message.
const runtimeSource = `// Client runs generated operations. It holds no connection and opens none:
// every method takes the handle to run on, so the caller decides whether the
// statement runs on a pool, a connection, or inside a transaction of its own.
type Client struct{}

// New returns a Client. It takes no arguments because there is no state to
// give it.
func New() *Client { return &Client{} }

// gopgqlDecoder widens a scanned value into the Go type a generated field
// declares, reporting whether it could.
type gopgqlDecoder[T any] func(any) (T, bool)

// gopgqlValue reads a value a non-null field declares. A null here is a schema
// violation rather than a missing value, so it is reported with the field's
// path.
func gopgqlValue[T any](path string, v any, decode gopgqlDecoder[T]) (T, error) {
	var zero T
	if v == nil {
		return zero, fmt.Errorf("gopgql: %s: null in a non-null field", path)
	}
	out, ok := decode(v)
	if !ok {
		return zero, fmt.Errorf("gopgql: %s: cannot read %T as %T", path, v, zero)
	}
	return out, nil
}

// gopgqlPointer reads a value a nullable field declares. A null stays nil, which
// is what keeps "no value" distinguishable from the zero value.
func gopgqlPointer[T any](path string, v any, decode gopgqlDecoder[T]) (*T, error) {
	if v == nil {
		return nil, nil
	}
	out, err := gopgqlValue(path, v, decode)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// gopgqlSlice reads an array column.
func gopgqlSlice[T any](path string, v any, decode gopgqlDecoder[T]) ([]T, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("gopgql: %s: expected an array, got %T", path, v)
	}
	out := make([]T, 0, len(raw))
	for i, item := range raw {
		el, err := gopgqlValue(fmt.Sprintf("%s[%d]", path, i), item, decode)
		if err != nil {
			return nil, err
		}
		out = append(out, el)
	}
	return out, nil
}

// gopgqlAsNumber reads a value that arrived as digits rather than as a Go
// number: the json.Number every Int, Float and numeric leaf is canonicalised to,
// and the pgtype.Numeric a numeric-returning function hands back unshaped.
//
// The latter is reached through json.Marshaler rather than named outright. That
// is how gopgql's own shaper reads it, for the same reason: naming pgtype here
// would put a database dependency in every generated client, including one whose
// schema has no numeric column anywhere.
//
// A marshaller that produced a JSON *string* produced "NaN" or "Infinity" —
// PostgreSQL's rendering of a value JSON has no number for. It is not a number
// here either, and saying so is what keeps it an error rather than a zero.
func gopgqlAsNumber(v any) (json.Number, bool) {
	switch t := v.(type) {
	case json.Number:
		return t, true
	case json.Marshaler:
		b, err := t.MarshalJSON()
		if err != nil || len(b) == 0 || b[0] == '"' {
			return "", false
		}
		return json.Number(b), true
	}
	return "", false
}

func gopgqlAsInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int16:
		return int64(n), true
	case int:
		return int64(n), true
	}
	// A fractional value reaches Int64 as an error rather than as a truncation,
	// which is the outcome a field declared Int should have.
	if n, ok := gopgqlAsNumber(v); ok {
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func gopgqlAsFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	// A numeric read into a Float is exact only as far as a float64 goes: the
	// canonical form carries the database's own digits, and 19.90 lands here as
	// 19.9. That loss is in the field's declared type, not in this decode.
	if n, ok := gopgqlAsNumber(v); ok {
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func gopgqlAsString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func gopgqlAsBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// gopgqlAsTime reads a DateTime, whose canonical response form is text and whose
// unshaped form is a time.Time.
//
// The text is RFC3339Nano **in UTC**: shape converts it there so that the same
// row does not come back with a different offset on a connection whose session
// TimeZone happens to differ. Parsing it back yields the same instant, which is
// what the field means; it is not the same Location, which the field does not.
func gopgqlAsTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, t)
		return parsed, err == nil
	}
	return time.Time{}, false
}

// gopgqlAsAny is the decoder for JSON, whose whole point is that its shape is
// not known statically. It accepts anything except the null a caller of
// gopgqlValue has already rejected.
func gopgqlAsAny(v any) (any, bool) { return v, true }

`
