package client

// runtimeSource is the fixed part of every generated client: the Client itself
// and the handful of decoders the generated assignments call.
//
// It is emitted into the generated package rather than imported from gopgql, so
// that the generated code depends on gopgql for the compiler and exec types it
// genuinely needs and for nothing else. These are eight small functions; a
// package to hold them would be a package to version.
//
// The decoders widen rather than assert. pgx decides what Go type a column comes
// back as, and `integer` arriving as int32 where the field is int64 is a
// difference in the driver, not in the schema — asserting on the exact type
// would turn that into an error at the caller. What is *not* widened is a
// mismatch that means something: a null in a non-null field, or a value of a
// kind the field cannot hold, both stop with the field's path in the message.
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

func gopgqlAsTime(v any) (time.Time, bool) {
	t, ok := v.(time.Time)
	return t, ok
}

// gopgqlAsAny is the decoder for JSON, whose whole point is that its shape is
// not known statically. It accepts anything except the null a caller of
// gopgqlValue has already rejected.
func gopgqlAsAny(v any) (any, bool) { return v, true }

`
