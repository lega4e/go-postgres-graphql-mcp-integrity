// This file is hand-written, and it is the only one in this package that is.
//
// It exists because the defect it covers lives in a seam nothing else could
// reach. shape canonicalises every leaf on the way out — an Int becomes a
// json.Number, a DateTime becomes RFC3339Nano text — and the generated decoders
// read whatever it produced. Both halves were individually correct and
// individually tested, and from v0.2.0 to v0.2.1 neither test executed the
// other's output: every decode site in this client was gopgqlAsString.
//
// The decoders are unexported, which is why the test is in package gen rather
// than beside the suite in test/m14. What it costs is this comment; what it buys
// is the join, asserted under **both** shaping strategies without a container,
// so a regression fails in seconds rather than in a consumer's build.
package gen

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/shape"
)

// seenAt is the timestamp both strategies carry, written in a non-UTC offset
// because that is what a session TimeZone produces and what the canonical form
// normalises away.
var seenAt = time.Date(2020, 1, 2, 3, 4, 5, 0, time.FixedZone("+02:00", 2*60*60))

// personID is the row's uuid in both the form pgx scans it (a raw [16]byte) and
// the 8-4-4-4-12 text the canonical response carries.
//
// The raw form is used below because shape.normaliseID accepts it and this is
// the cheapest place to hold it to that. In a live query it would already be
// text by then: exec.scan renders every [16]byte through jsonValue before shape
// or any decoder sees it. Both conversions exist, which is why an ID reaches
// gopgqlAsString as a string on either path.
const personIDText = "9f1c0d0e-0000-4000-8000-000000000001"

var personIDBytes = [16]byte{
	0x9f, 0x1c, 0x0d, 0x0e, 0x00, 0x00, 0x40, 0x00,
	0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01,
}

// ticketBeyondFloat64 is 2^53+1, the smallest integer a float64 cannot hold. A
// decoder that reached int64 by way of a float would return ...992 for it, so it
// is the value that makes "read from the digits" falsifiable rather than stated.
const ticketBeyondFloat64 = int64(9007199254740993)

// notesJSON is the embedded document, carrying a trailing zero so that shape's
// UseNumber decode is asserted inside a JSON leaf too: 19.90 must not become 19.9.
const notesJSON = `{"note":"measured","exact":19.90}`

// goSideRow is one flat row close to the form pgx scans it: `uuid` as [16]byte,
// `integer` as int32, `numeric` as pgtype.Numeric, `timestamptz` as time.Time,
// `integer[]` as []int32, and a jsonb projected ::text as a string. Not one of
// those is the form the response carries, which is the point.
func goSideRow(t *testing.T) map[string]any {
	t.Helper()
	var rating pgtype.Numeric
	require.NoError(t, rating.Scan("12.50"))
	return map[string]any{
		"v0_k":   personIDText,
		"v0_c0":  personIDBytes,
		"v0_c1":  "Ada",
		"v0_c2":  int32(41),
		"v0_c3":  9.5,
		"v0_c4":  rating,
		"v0_c5":  seenAt,
		"v0_c6":  []int32{1, 2, 3},
		"v0_c7":  int32(82),
		"v0_c8":  ticketBeyondFloat64,
		"v0_c9":  true,
		"v0_c10": notesJSON,
	}
}

// sqlSideJSON is the same row as the SQL-side strategy returns it: one JSON
// document the database assembled, decoded with UseNumber.
const sqlSideJSON = `{"persons":[{"id":"` + personIDText + `","name":"Ada","age":41,` +
	`"score":9.5,"rating":12.50,"seen":"2020-01-02T03:04:05+02:00","marks":[1,2,3],` +
	`"tally":82,"ticket":9007199254740993,"active":true,` +
	`"notes":"{\"note\":\"measured\",\"exact\":19.90}"}]}`

// TestGeneratedDecodersReadTheCanonicalResponse is the regression test for #51.
//
// Every scalar here reaches the decoder as something other than the Go type pgx
// scanned: json.Number for the Int, the Float and the numeric-backed Float,
// RFC3339Nano text for the DateTime, and a list of json.Number for the Int list.
// Before the fix each of those failed with "cannot read json.Number as int64" or
// "cannot read string as time.Time", on both strategies.
func TestGeneratedDecodersReadTheCanonicalResponse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(t *testing.T) (map[string]any, error)
	}{
		{
			name: "Go-side shaping",
			shape: func(t *testing.T) (map[string]any, error) {
				return shape.Rows(findMeasurementsProjection, []map[string]any{goSideRow(t)})
			},
		},
		{
			name: "SQL-side shaping",
			shape: func(*testing.T) (map[string]any, error) {
				return shape.Decode(findMeasurementsProjection, sqlSideJSON)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.shape(t)
			require.NoError(t, err)

			people, err := assembleFindMeasurementsPerson("persons", res["persons"])
			require.NoError(t, err, "the generated decoders must read what the shaper produced")
			require.Len(t, people, 1)

			assert.Equal(t, personIDText, people[0].Id,
				"an ID arrives as uuid text, whichever form the producer held it in")
			assert.Equal(t, "Ada", people[0].Name)
			assert.Equal(t, int64(41), people[0].Age, "an Int arrives as json.Number")
			assert.Equal(t, 9.5, people[0].Score, "a Float arrives as json.Number")
			require.NotNil(t, people[0].Rating)
			assert.Equal(t, 12.5, *people[0].Rating,
				"a numeric-backed Float arrives as a json.Number of the database's own digits")
			require.NotNil(t, people[0].Seen)
			assert.True(t, seenAt.Equal(*people[0].Seen),
				"a DateTime arrives as RFC3339Nano text in UTC, and must decode back to the same instant")
			assert.Equal(t, []int64{1, 2, 3}, people[0].Marks, "an Int list arrives elementwise")
			require.NotNil(t, people[0].Tally)
			assert.Equal(t, int64(82), *people[0].Tally, "so does a nullable Int that has a value")
			assert.Equal(t, ticketBeyondFloat64, people[0].Ticket,
				"an Int beyond float64 precision survives, because the digits are what is read")
			assert.True(t, people[0].Active)
			require.NotNil(t, people[0].Notes)
			assert.Equal(t, map[string]any{"note": "measured", "exact": json.Number("19.90")},
				*people[0].Notes, "a JSON document arrives decoded, with its own digits intact")
		})
	}
}

// The two strategies must hand the decoders the *same* values, not merely values
// each half can read. That is M8's guarantee restated at the point where it is
// actually consumed, and it is what makes one fix cover both paths.
func TestBothStrategiesDecodeToTheSameStruct(t *testing.T) {
	goSide, err := shape.Rows(findMeasurementsProjection, []map[string]any{goSideRow(t)})
	require.NoError(t, err)
	sqlSide, err := shape.Decode(findMeasurementsProjection, sqlSideJSON)
	require.NoError(t, err)
	assert.Equal(t, goSide, sqlSide, "the canonical response is the same value on both paths")

	fromGo, err := assembleFindMeasurementsPerson("persons", goSide["persons"])
	require.NoError(t, err)
	fromSQL, err := assembleFindMeasurementsPerson("persons", sqlSide["persons"])
	require.NoError(t, err)
	assert.Equal(t, fromGo, fromSQL)
}

// TestEveryDecoderReadsBothProducers is the class of defect #51 belongs to,
// written down: a decoder has two producers, and reading only one of them is a
// bug the other's tests cannot see.
//
// exec.Query's values have been canonicalised by shape — json.Number for Int,
// Float and numeric, RFC3339Nano text for DateTime — while exec.Call's have been
// through nothing and arrive as pgx scanned them. Every decoder is checked
// against both, and against the values it must still refuse.
func TestEveryDecoderReadsBothProducers(t *testing.T) {
	numeric := func(s string) pgtype.Numeric {
		var n pgtype.Numeric
		require.NoError(t, n.Scan(s))
		return n
	}

	t.Run("gopgqlAsInt64", func(t *testing.T) {
		for _, v := range []any{int64(42), int32(42), int16(42), 42, json.Number("42"), numeric("42")} {
			got, ok := gopgqlAsInt64(v)
			assert.True(t, ok, "%T", v)
			assert.Equal(t, int64(42), got, "%T", v)
		}
		for _, v := range []any{json.Number("41.5"), numeric("41.5"), numeric("NaN"), "42", true, 4.2} {
			_, ok := gopgqlAsInt64(v)
			assert.False(t, ok, "%T %v must not be truncated or coerced into an Int", v, v)
		}
	})

	t.Run("gopgqlAsFloat64", func(t *testing.T) {
		for _, v := range []any{9.5, float32(9.5), json.Number("9.5"), numeric("9.50")} {
			got, ok := gopgqlAsFloat64(v)
			assert.True(t, ok, "%T", v)
			assert.Equal(t, 9.5, got, "%T", v)
		}
		for _, v := range []any{numeric("NaN"), "9.5", true} {
			_, ok := gopgqlAsFloat64(v)
			assert.False(t, ok, "%T %v has no float64 a caller could use", v, v)
		}
	})

	t.Run("gopgqlAsTime", func(t *testing.T) {
		// shape renders a DateTime in UTC; exec.Call hands back the time.Time.
		for _, v := range []any{seenAt, "2020-01-02T01:04:05Z", "2020-01-02T01:04:05.000Z"} {
			got, ok := gopgqlAsTime(v)
			require.True(t, ok, "%T %v", v, v)
			assert.True(t, seenAt.Equal(got), "%v", v)
		}
		for _, v := range []any{"2 January 2020", "", int64(0)} {
			_, ok := gopgqlAsTime(v)
			assert.False(t, ok, "%v is not a DateTime", v)
		}
	})

	// String, Boolean and JSON reach their decoder as the same Go type pgx
	// scanned, so those three have one producer rather than two. Asserted anyway,
	// because "this decoder has only one producer" is the belief that was wrong
	// about Int, and it is worth failing here if shape's table ever changes.
	//
	// ID looks like the exception and is not, for a reason worth writing down
	// because it is not visible from here: pgx scans a uuid as [16]byte, which
	// gopgqlAsString would refuse. Neither producer ever hands one over. The
	// shaped path converts it in shape.normaliseID, and the *unshaped* path
	// converts it too — exec.Call and exec.Query share exec.scan, whose jsonValue
	// renders any [16]byte as 8-4-4-4-12 text before a decoder sees it. So the
	// raw form is refused here on purpose: it is exec's to convert, and a
	// decoder that quietly accepted it would also make a String field accept
	// sixteen bytes, which is a mismapped column rather than an ID.
	t.Run("the scalars whose canonical form is already the Go type", func(t *testing.T) {
		s, ok := gopgqlAsString(personIDText)
		assert.True(t, ok)
		assert.Equal(t, personIDText, s)
		_, ok = gopgqlAsString(json.Number("42"))
		assert.False(t, ok, "a number is not a String, and saying so keeps a mismapped column findable")
		_, ok = gopgqlAsString(personIDBytes)
		assert.False(t, ok, "a raw uuid is exec.scan's to render, and no producer hands one to a decoder")

		b, ok := gopgqlAsBool(true)
		assert.True(t, ok)
		assert.True(t, b)
		_, ok = gopgqlAsBool(json.Number("1"))
		assert.False(t, ok, "a Boolean is canonicalised as a bool, so a number here is a mismapped column")

		// JSON is decoded with UseNumber, so its leaves are json.Number too —
		// which gopgqlAsAny passes through untouched, by design.
		v, ok := gopgqlAsAny(map[string]any{"n": json.Number("19.90")})
		assert.True(t, ok)
		assert.Equal(t, map[string]any{"n": json.Number("19.90")}, v)
	})
}

// A value the field genuinely cannot hold still stops, with the field's path in
// the message. Widening the decoders must not turn a schema mistake into a
// silently truncated number.
func TestAFractionalNumberIsStillRefusedForAnInt(t *testing.T) {
	res, err := shape.Decode(findMeasurementsProjection,
		`{"persons":[{"id":"`+personIDText+`","name":"Ada","age":41.5,"score":9.5,"rating":null,`+
			`"seen":null,"marks":null,"tally":null,"ticket":1,"active":false,"notes":null}]}`)
	require.NoError(t, err)

	_, err = assembleFindMeasurementsPerson("persons", res["persons"])
	require.ErrorContains(t, err, "persons[0].age",
		"the failure names the field, not the decoder")
}
