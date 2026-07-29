// The comparison is tested here over hand-built schema.Schema pairs and no
// database at all — that separation is the reason design D4 made drift a diff
// of two values of one type. Reflect's SQL is exercised against a real
// postgres:19beta2 container in the M7 integration suite; every rule about
// which differences become which finding lives here, where a failure points at
// the logic rather than at a container.
package conform_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/conform"
	"github.com/lega4e/gopgql/schema"
)

// person is the reference vertex both sides start from: one table, one label,
// three properties. Each test perturbs a copy of it in exactly one way, so the
// finding under test is the only one a clean run can produce.
func person() schema.VertexTable {
	return schema.VertexTable{
		Name:       "person",
		Label:      "person",
		Columns:    []schema.Column{{Name: "id", Type: "uuid", PrimaryKey: true}},
		Properties: []string{"id", "name", "email"},
	}
}

func graphOf(vertices []schema.VertexTable, edges []schema.EdgeTable) *schema.Schema {
	return &schema.Schema{GraphName: "app_graph", VertexTables: vertices, EdgeTables: edges}
}

func TestCheckCleanDatabaseHasNoFindings(t *testing.T) {
	desired := graphOf([]schema.VertexTable{person()}, nil)
	actual := graphOf([]schema.VertexTable{person()}, nil)

	report := conform.Check(desired, actual)

	assert.True(t, report.OK())
	assert.Empty(t, report.Findings)
}

func TestCheckReportsMissingElement(t *testing.T) {
	desired := graphOf([]schema.VertexTable{person()}, nil)
	actual := graphOf(nil, nil)

	report := conform.Check(desired, actual)

	require.Len(t, report.Findings, 1)
	assert.False(t, report.OK())
	assert.Equal(t, conform.Finding{
		Kind:    conform.MissingElement,
		Element: "person",
		Want:    "person",
	}, report.Findings[0])
}

func TestCheckReportsUnexpectedElement(t *testing.T) {
	desired := graphOf(nil, nil)
	actual := graphOf([]schema.VertexTable{person()}, nil)

	report := conform.Check(desired, actual)

	require.Len(t, report.Findings, 1)
	assert.Equal(t, conform.Finding{
		Kind:    conform.UnexpectedElement,
		Element: "person",
		Got:     "person",
	}, report.Findings[0])
}

// The property case is the one the integration suite injects deliberately: drop
// a single property from the live graph and the report has to name it, its
// element, and that the database is the side missing it.
func TestCheckReportsMissingProperty(t *testing.T) {
	drifted := person()
	drifted.Properties = []string{"id", "name"}

	report := conform.Check(
		graphOf([]schema.VertexTable{person()}, nil),
		graphOf([]schema.VertexTable{drifted}, nil),
	)

	require.Len(t, report.Findings, 1)
	assert.Equal(t, conform.Finding{
		Kind:     conform.MissingProperty,
		Element:  "person",
		Property: "email",
		Want:     "email",
	}, report.Findings[0])
}

func TestCheckReportsUnexpectedProperty(t *testing.T) {
	drifted := person()
	drifted.Properties = append(drifted.Properties, "secret")

	report := conform.Check(
		graphOf([]schema.VertexTable{person()}, nil),
		graphOf([]schema.VertexTable{drifted}, nil),
	)

	require.Len(t, report.Findings, 1)
	assert.Equal(t, conform.Finding{
		Kind:     conform.UnexpectedProperty,
		Element:  "person",
		Property: "secret",
		Got:      "secret",
	}, report.Findings[0])
}

// A label disagreement names both labels, so the report says what the SDL asked
// for and what the database has rather than only that they differ.
func TestCheckReportsLabelMismatch(t *testing.T) {
	drifted := person()
	drifted.Label = "people"

	report := conform.Check(
		graphOf([]schema.VertexTable{person()}, nil),
		graphOf([]schema.VertexTable{drifted}, nil),
	)

	require.Len(t, report.Findings, 1)
	assert.Equal(t, conform.Finding{
		Kind:    conform.LabelMismatch,
		Element: "person",
		Want:    "person",
		Got:     "people",
	}, report.Findings[0])
}

// A shared label dropped out of band is a label mismatch, not an element one:
// the table is still there, it just stopped answering to the interface's label
// (SPEC.md §7 → M4). The properties it exposed under that label go with it.
func TestCheckReportsADroppedSharedLabel(t *testing.T) {
	desired := person()
	desired.ExtraLabels = []schema.LabelProperties{
		{Label: "actor", Properties: []string{"id", "name"}},
	}

	report := conform.Check(
		graphOf([]schema.VertexTable{desired}, nil),
		graphOf([]schema.VertexTable{person()}, nil),
	)

	require.Len(t, report.Findings, 1)
	assert.Equal(t, conform.Finding{
		Kind:    conform.LabelMismatch,
		Element: "person",
		Want:    "actor, person",
		Got:     "person",
	}, report.Findings[0])
}

// Properties are pooled across an element's labels, because Finding names a
// property by element and has no label field. A property exposed only under a
// shared label still counts as present.
func TestCheckPoolsPropertiesAcrossLabels(t *testing.T) {
	build := func() schema.VertexTable {
		vt := person()
		vt.Properties = []string{"id", "name"}
		vt.ExtraLabels = []schema.LabelProperties{
			{Label: "actor", Properties: []string{"id", "email"}},
		}
		return vt
	}

	report := conform.Check(
		graphOf([]schema.VertexTable{build()}, nil),
		graphOf([]schema.VertexTable{build()}, nil),
	)

	assert.True(t, report.OK())
	assert.Empty(t, report.Findings)
}

// Edge tables are elements too, and drift on one is reported the same way.
func TestCheckReportsEdgeDrift(t *testing.T) {
	edge := func(props ...string) schema.EdgeTable {
		return schema.EdgeTable{
			Name: "person_follows", Label: "follows",
			SourceKey: "source_id", SourceTable: "person", SourceRef: "id",
			DestKey: "target_id", DestTable: "person", DestRef: "id",
			Properties: props,
		}
	}

	report := conform.Check(
		graphOf([]schema.VertexTable{person()}, []schema.EdgeTable{edge("source_id", "target_id", "since")}),
		graphOf([]schema.VertexTable{person()}, []schema.EdgeTable{edge("source_id", "target_id")}),
	)

	require.Len(t, report.Findings, 1)
	assert.Equal(t, conform.Finding{
		Kind:     conform.MissingProperty,
		Element:  "person_follows",
		Property: "since",
		Want:     "since",
	}, report.Findings[0])
}

// Declaration order is not drift: the catalogs record a set, and the generator's
// order is an artefact of the SDL. Comparing sets is what keeps a reordered SDL
// from lighting up a conformance run.
func TestCheckIgnoresDeclarationOrder(t *testing.T) {
	reordered := person()
	reordered.Properties = []string{"email", "id", "name"}
	reordered.ExtraLabels = nil

	report := conform.Check(
		graphOf([]schema.VertexTable{person()}, nil),
		graphOf([]schema.VertexTable{reordered}, nil),
	)

	assert.True(t, report.OK())
}

// The reflected side cannot carry columns or indexes, so Check must not compare
// them — otherwise every run would report drift that is not there. This is the
// assertion that pins the package doc's "graph drift only" claim to behaviour.
func TestCheckIgnoresFieldsReflectionCannotFill(t *testing.T) {
	desired := person()
	desired.Columns = []schema.Column{
		{Name: "id", Type: "uuid", PrimaryKey: true},
		{Name: "name", Type: "text", NotNull: true, Default: "'anonymous'"},
		{Name: "email", Type: "text", Unique: true},
	}

	reflected := person()
	reflected.Columns = nil

	full := graphOf([]schema.VertexTable{desired}, nil)
	full.Indexes = []schema.Index{{Name: "person_name_idx", Table: "person", Columns: []string{"name"}}}

	report := conform.Check(full, graphOf([]schema.VertexTable{reflected}, nil))

	assert.True(t, report.OK())
	assert.Empty(t, report.Findings)
}

// Every kind at once, to pin the report's order. Findings come out by element
// name, and within an element the label disagreement first, then the missing
// properties, then the unexpected ones — so two runs over the same pair are
// byte-identical and a CI diff of two reports means something.
func TestCheckFindingsAreOrderedDeterministically(t *testing.T) {
	desiredPerson := person()
	actualPerson := person()
	actualPerson.Label = "people"
	actualPerson.Properties = []string{"id", "name", "nickname"}

	desired := graphOf([]schema.VertexTable{
		{Name: "account", Label: "account", Properties: []string{"id"}},
		desiredPerson,
	}, nil)
	actual := graphOf([]schema.VertexTable{
		actualPerson,
		{Name: "zombie", Label: "zombie", Properties: []string{"id"}},
	}, nil)

	report := conform.Check(desired, actual)

	assert.Equal(t, []conform.Finding{
		{Kind: conform.MissingElement, Element: "account", Want: "account"},
		{Kind: conform.LabelMismatch, Element: "person", Want: "person", Got: "people"},
		{Kind: conform.MissingProperty, Element: "person", Property: "email", Want: "email"},
		{Kind: conform.UnexpectedProperty, Element: "person", Property: "nickname", Got: "nickname"},
		{Kind: conform.UnexpectedElement, Element: "zombie", Got: "zombie"},
	}, report.Findings)
}

// A caller branches on the kind. Nothing in this loop reads a message, which is
// the whole point of a typed finding (design D4).
func TestFindingsAreDistinguishableByKind(t *testing.T) {
	report := conform.Check(
		graphOf([]schema.VertexTable{person(), {Name: "account", Label: "account", Properties: []string{"id"}}}, nil),
		graphOf([]schema.VertexTable{{Name: "person", Label: "person", Properties: []string{"id", "name", "extra"}}}, nil),
	)

	byKind := map[conform.Kind]int{}
	for _, f := range report.Findings {
		switch f.Kind {
		case conform.MissingElement, conform.UnexpectedElement,
			conform.MissingProperty, conform.UnexpectedProperty, conform.LabelMismatch:
			byKind[f.Kind]++
		default:
			t.Fatalf("unrecognised finding kind %q", f.Kind)
		}
	}

	assert.Equal(t, map[conform.Kind]int{
		conform.MissingElement:     1,
		conform.MissingProperty:    1,
		conform.UnexpectedProperty: 1,
	}, byKind)
}

// A nil model is treated as one with no elements rather than panicking: Reflect
// returns a nil schema alongside an error, and a caller that checks the report
// before the error should get a useless answer, not a crash.
func TestCheckHandlesNilSchemas(t *testing.T) {
	assert.True(t, conform.Check(nil, nil).OK())

	fromNilDesired := conform.Check(nil, graphOf([]schema.VertexTable{person()}, nil))
	require.Len(t, fromNilDesired.Findings, 1)
	assert.Equal(t, conform.UnexpectedElement, fromNilDesired.Findings[0].Kind)

	fromNilActual := conform.Check(graphOf([]schema.VertexTable{person()}, nil), nil)
	require.Len(t, fromNilActual.Findings, 1)
	assert.Equal(t, conform.MissingElement, fromNilActual.Findings[0].Kind)
}
