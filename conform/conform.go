// Package conform checks that the property graph a database actually holds is
// still the one the SDL describes.
//
// SPEC.md §3.1 states the assumption the whole migration story rests on: prior
// state is reconstructed by folding gopgql's own migrations in memory, and that
// is only sound because "no one hand-edits a generated migration or alters the
// database out of band". Nothing else in gopgql enforces it. The generator, the
// differ and the compiler all reason from the SDL alone, so they would go on
// agreeing with each other indefinitely while the database quietly diverged —
// and the first symptom would be a query compiled against a mapping that no
// longer exists. SPEC.md §1 names this check as the guard for that gap; this
// package is it.
//
// Reflect reads the live graph out of the pg_propgraph_* catalogs into the same
// [schema.Schema] the generator builds from SDL, and Check diffs the two
// (design D4). Because both sides are values of one type, drift is a comparison
// rather than a model-versus-database special case, and the result is a Report
// of typed Findings: a caller branches on [Finding.Kind] instead of parsing
// English out of a message.
//
// # What this check does not cover
//
// It compares the property graph, and only the property graph: which elements
// exist, what labels they carry, and which properties are exposed under them.
// That is the entirety of what pg_propgraph_element, pg_propgraph_label and
// pg_propgraph_property record. Table-level objects live in other catalogs and
// are therefore NOT checked:
//
//   - column defaults (@default)
//   - CHECK constraints (@check)
//   - UNIQUE constraints, including the natural key (@key)
//   - indexes (@index)
//   - column types, nullability, primary keys and foreign keys
//   - tables and columns the graph does not expose at all
//
// So an empty Report means the graph mapping matches the SDL. It does not mean
// the tables underneath it do: someone can drop a CHECK constraint or change a
// column's default and this check will still pass. Closing that gap needs
// information_schema reflected alongside the graph catalogs, which the design
// records as an open question rather than an M7 deliverable. Overstating the
// coverage would be worse than the gap, because an operator would stop looking.
//
// Reflection needs a live connection, so conform sits on the pgx side of the
// WASM boundary alongside exec. sdl, schema, generator, migrate and compiler
// have no database dependency and must not import this package (SPEC.md §4.1,
// design D5).
package conform

import (
	"slices"
	"strings"

	"github.com/lega4e/gopgql/schema"
)

// Kind classifies a Finding. It is the axis a caller branches on: every finding
// carries one, and the five values below are exhaustive, so a CI step or a
// repair tool can switch on the kind without reading Want, Got or any prose.
type Kind string

const (
	// MissingElement: the SDL declares an element the graph does not expose.
	// Want holds the labels the SDL declares for it; Got is empty.
	MissingElement Kind = "MissingElement"

	// UnexpectedElement: the graph exposes an element the SDL does not
	// declare. Got holds the labels the database has for it; Want is empty.
	UnexpectedElement Kind = "UnexpectedElement"

	// MissingProperty: an element exists on both sides, but a property the SDL
	// declares is not exposed by the graph. This is the shape out-of-band drift
	// usually takes, because dropping one property is easier than dropping a
	// whole element.
	MissingProperty Kind = "MissingProperty"

	// UnexpectedProperty: the graph exposes a property under an element that
	// the SDL does not declare there.
	UnexpectedProperty Kind = "UnexpectedProperty"

	// LabelMismatch: an element exists on both sides but carries a different
	// set of labels. Want and Got each name that side's labels, so the report
	// says what the SDL asked for and what the database has, not merely that
	// they differ.
	LabelMismatch Kind = "LabelMismatch"
)

// Finding is one difference between the SDL's graph and the database's.
//
// Want and Got follow one rule throughout: Want is what the SDL declares at
// that location and Got is what the database holds there, with an empty string
// meaning "nothing there". That keeps a finding readable without a lookup table
// of per-kind conventions.
type Finding struct {
	// Kind is what sort of drift this is. Branch on it.
	Kind Kind
	// Element is the element the finding concerns — the table name, which is
	// how [schema.VertexTable] and [schema.EdgeTable] identify themselves.
	Element string
	// Property is the property the finding concerns, empty for findings about
	// the element itself (MissingElement, UnexpectedElement, LabelMismatch).
	Property string
	// Want is what the SDL declares; empty when the SDL declares nothing here.
	Want string
	// Got is what the database holds; empty when the database holds nothing
	// here.
	Got string
}

// Report is the whole outcome of a check. It is a struct rather than a bare
// slice so that later milestones can add summary fields without breaking every
// caller's signature.
type Report struct {
	// Findings are in a stable order: by element name, and within an element
	// the label disagreement first, then missing properties, then unexpected
	// ones, each alphabetically. Two runs over the same pair of schemas produce
	// byte-identical reports, so a CI diff of two reports is meaningful.
	Findings []Finding
}

// OK reports whether the database conforms — no findings at all. It is the
// check the CLI's exit status is built on (design D5).
func (r Report) OK() bool { return len(r.Findings) == 0 }

// Check compares the graph the SDL describes against the graph the database
// holds and returns the differences as structured findings.
//
// desired is what the generator built from SDL; actual is what Reflect read
// back. Both are ordinary values, which is the point of design D4 — the
// comparison logic has no database in it and is tested without one.
//
// Only the three axes the catalogs expose are compared: elements, their labels,
// and their properties. Columns, indexes, keys and constraints carried by
// [schema.Schema] are deliberately ignored, because Reflect cannot populate
// them from pg_propgraph_* and a comparison of a populated field against an
// unpopulated one would report drift that is not there. The package doc lists
// what that leaves uncovered.
//
// Order does not matter on either side: labels and properties are compared as
// sets, since the catalogs record no order and SDL declaration order is not
// drift. A nil schema is treated as one with no elements, so Check(nil, actual)
// reports every element in the database as unexpected rather than panicking.
func Check(desired, actual *schema.Schema) Report {
	want, got := view(desired), view(actual)

	names := make([]string, 0, len(want)+len(got))
	for name := range want {
		names = append(names, name)
	}
	for name := range got {
		if _, both := want[name]; !both {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	var findings []Finding
	for _, name := range names {
		w, declared := want[name]
		g, present := got[name]

		switch {
		case !present:
			// One finding for the element, not one per property it took with
			// it: the element's absence is the fact, and a caller drowning in
			// per-property noise would have to reconstruct it anyway.
			findings = append(findings, Finding{
				Kind: MissingElement, Element: name, Want: strings.Join(w.labels, ", "),
			})
			continue
		case !declared:
			findings = append(findings, Finding{
				Kind: UnexpectedElement, Element: name, Got: strings.Join(g.labels, ", "),
			})
			continue
		}

		if !slices.Equal(w.labels, g.labels) {
			findings = append(findings, Finding{
				Kind:    LabelMismatch,
				Element: name,
				Want:    strings.Join(w.labels, ", "),
				Got:     strings.Join(g.labels, ", "),
			})
		}
		for _, p := range missing(w.properties, g.properties) {
			findings = append(findings, Finding{
				Kind: MissingProperty, Element: name, Property: p, Want: p,
			})
		}
		for _, p := range missing(g.properties, w.properties) {
			findings = append(findings, Finding{
				Kind: UnexpectedProperty, Element: name, Property: p, Got: p,
			})
		}
	}
	return Report{Findings: findings}
}

// elementView is one element reduced to the three axes pg_propgraph_* records.
// Reducing both sides to it before comparing is what keeps Check from tripping
// over fields Reflect cannot fill.
type elementView struct {
	// labels are every label the element carries, sorted and deduplicated.
	// A vertex table's own label and its shared interface labels (SPEC.md §7 →
	// M4) are pooled here on purpose: the catalogs record a set of labels per
	// element and nothing that says which one the table "owns", so treating one
	// as primary would make the comparison depend on a distinction the database
	// does not have.
	labels []string
	// properties are every property exposed on the element, pooled across its
	// labels, sorted and deduplicated. Finding has no label field, so a
	// property is reported against its element.
	properties []string
}

// view reduces a schema to its comparable elements, keyed by table name.
func view(m *schema.Schema) map[string]elementView {
	out := make(map[string]elementView)
	if m == nil {
		return out
	}
	for _, vt := range m.VertexTables {
		labels := make([]string, 0, 1+len(vt.ExtraLabels))
		labels = append(labels, vt.Label)
		props := slices.Clone(vt.Properties)
		for _, extra := range vt.ExtraLabels {
			labels = append(labels, extra.Label)
			props = append(props, extra.Properties...)
		}
		out[vt.Name] = elementView{labels: sortedSet(labels), properties: sortedSet(props)}
	}
	for _, et := range m.EdgeTables {
		out[et.Name] = elementView{
			labels:     sortedSet([]string{et.Label}),
			properties: sortedSet(et.Properties),
		}
	}
	return out
}

// sortedSet returns the distinct non-empty members of in, sorted. Empty strings
// are dropped so that an unlabelled element does not compare as one carrying a
// label named "".
func sortedSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// missing returns the members of a that are absent from b. Both are sorted
// sets, so the result is sorted too and the findings come out deterministically.
func missing(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}
