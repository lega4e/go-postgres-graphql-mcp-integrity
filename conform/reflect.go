package conform

import (
	"context"
	"errors"
	"fmt"

	"github.com/lega4e/gopgql/exec"
	"github.com/lega4e/gopgql/generator"
	"github.com/lega4e/gopgql/schema"
)

// ErrGraphNotFound reports that the named property graph does not exist in the
// database.
//
// It is a distinct error rather than an empty schema on purpose. Reflect's
// result feeds straight into Check, and an empty model compared against the SDL
// produces a MissingElement for every element in the schema — a report that
// reads as catastrophic drift when the truth is usually mundane: the migrations
// were never applied, the graph name is misspelled, or the connection points at
// the wrong database. Those are different problems with different fixes, and
// only a named error keeps them distinguishable (design D4; the conformance
// spec's "No graph" scenario).
//
// A server with no SQL/PGQ support at all also lands here, since it has no
// relation of the property-graph kind to find.
var ErrGraphNotFound = errors.New("conform: property graph not found")

// Reflect reads the property graph the database actually holds into the same
// [schema.Schema] the generator produces from SDL, so the two can be compared
// as values of one type (design D4).
//
// It reads the graph catalogs of PostgreSQL's SQL/PGQ implementation:
// pg_propgraph_element for the elements and their edge keys,
// pg_propgraph_label and pg_propgraph_element_label for the labels each element
// carries, and pg_propgraph_property with pg_propgraph_label_property for the
// properties exposed under each of those labels. The last two link tables are
// how this PostgreSQL normalises labels and properties across a graph; they
// carry no information of their own beyond joining the three catalogs SPEC.md
// §1 names.
//
// db is any handle that can run a query — a *pgxpool.Pool, a *pgx.Conn or a
// pgx.Tx all satisfy [exec.Querier]. graphName defaults to
// [generator.DefaultGraphName] when empty, matching every other entry point.
//
// # What comes back, and what does not
//
// Only the fields the catalogs can populate are set: the element names, their
// labels, their properties, and an edge's source and destination tables and key
// columns. Columns and Indexes are left nil, and so are every column-level flag
// on them, because pg_propgraph_* does not record them — see the package doc
// for the full list of what that leaves unchecked. Check compares only the
// fields Reflect can fill, so the gaps do not read as drift.
//
// Two representational notes, both of which Check is written around:
//
//   - The catalogs record a set of labels per element with nothing marking one
//     as the table's own, while [schema.VertexTable] splits them into Label and
//     ExtraLabels. Reflect fills Label with the alphabetically first label and
//     ExtraLabels with the rest. Check compares the pooled set, so the split
//     never affects the result — but a reflected VertexTable.Label is not
//     necessarily the label the SDL calls the table's own.
//   - [schema.EdgeTable] holds a single label and single-column source and
//     destination keys, which is all gopgql ever generates. A hand-made edge
//     with several labels or a multi-column key is reduced to the first of
//     each; the reduction can only ever understate what the database has, so it
//     cannot manufacture a clean report out of a dirty database.
func Reflect(ctx context.Context, db exec.Querier, graphName string) (*schema.Schema, error) {
	if db == nil {
		return nil, fmt.Errorf("conform: nil database handle")
	}
	if graphName == "" {
		graphName = generator.DefaultGraphName
	}

	graphID, err := graphOID(ctx, db, graphName)
	if err != nil {
		return nil, err
	}
	labels, err := reflectLabels(ctx, db, graphID)
	if err != nil {
		return nil, err
	}
	elements, err := reflectElements(ctx, db, graphID)
	if err != nil {
		return nil, err
	}

	m := &schema.Schema{GraphName: graphName}
	for _, el := range elements {
		switch el.kind {
		case elementKindVertex:
			vt := schema.VertexTable{Name: el.table, Schema: el.schemaName}
			if own := labels[el.key()]; len(own) > 0 {
				vt.Label = own[0].Label
				vt.Properties = own[0].Properties
				if len(own) > 1 {
					vt.ExtraLabels = own[1:]
				}
			}
			m.VertexTables = append(m.VertexTables, vt)
		case elementKindEdge:
			et := schema.EdgeTable{
				Name:         el.table,
				Schema:       el.schemaName,
				SourceSchema: el.sourceSchema,
				SourceTable:  el.sourceTable,
				SourceKey:    el.sourceKey,
				SourceRef:    el.sourceRef,
				DestSchema:   el.destSchema,
				DestTable:    el.destTable,
				DestKey:      el.destKey,
				DestRef:      el.destRef,
			}
			if own := labels[el.key()]; len(own) > 0 {
				et.Label = own[0].Label
				et.Properties = own[0].Properties
			}
			m.EdgeTables = append(m.EdgeTables, et)
		default:
			return nil, fmt.Errorf(
				"conform: element %q has unrecognised kind %q in graph %q",
				el.table, el.kind, graphName)
		}
	}
	return m, nil
}

// Element kinds as pg_propgraph_element.pgekind spells them (PGEKIND_VERTEX,
// PGEKIND_EDGE).
const (
	elementKindVertex = "v"
	elementKindEdge   = "e"
)

// graphOIDSQL finds the property graph by name. A property graph is a relation
// of its own kind ('g') in pg_class, so an unqualified name resolves through
// the session's search_path exactly as it does in a CREATE PROPERTY GRAPH or a
// GRAPH_TABLE — pg_table_is_visible is what makes the lookup agree with the
// rest of the session rather than matching a same-named graph in some other
// schema.
const graphOIDSQL = `
SELECT c.oid
FROM pg_catalog.pg_class c
WHERE c.relkind = 'g'
  AND c.relname = $1
  AND pg_catalog.pg_table_is_visible(c.oid)`

func graphOID(ctx context.Context, db exec.Querier, graphName string) (uint32, error) {
	rows, err := db.Query(ctx, graphOIDSQL, graphName)
	if err != nil {
		return 0, fmt.Errorf("conform: look up property graph %q: %w", graphName, err)
	}
	defer rows.Close()

	var oid uint32
	found := false
	for rows.Next() {
		if err := rows.Scan(&oid); err != nil {
			return 0, fmt.Errorf("conform: look up property graph %q: %w", graphName, err)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("conform: look up property graph %q: %w", graphName, err)
	}
	if !found {
		return 0, fmt.Errorf("%w: %q", ErrGraphNotFound, graphName)
	}
	return oid, nil
}

// labelsSQL collects, for every element of the graph, each label it carries and
// the properties exposed under that label.
//
// The property join is a LEFT JOIN so that a label exposing no properties still
// produces a row: a label that lost all of its properties out of band is drift
// worth reporting, and an INNER JOIN would silently drop the element instead.
const labelsSQL = `
SELECT n.nspname::text AS schema_name,
       c.relname::text AS table_name,
       l.pgllabel::text AS label,
       COALESCE(
           array_agg(p.pgpname::text ORDER BY p.pgpname)
               FILTER (WHERE p.pgpname IS NOT NULL),
           '{}'::text[]) AS properties
FROM pg_catalog.pg_propgraph_element_label el
JOIN pg_catalog.pg_propgraph_label l ON l.oid = el.pgellabelid
JOIN pg_catalog.pg_propgraph_element e ON e.oid = el.pgelelid
JOIN pg_catalog.pg_class c ON c.oid = e.pgerelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_propgraph_label_property lp ON lp.plpellabelid = el.oid
LEFT JOIN pg_catalog.pg_propgraph_property p ON p.oid = lp.plppropid
WHERE e.pgepgid = $1
GROUP BY n.nspname, c.relname, l.pgllabel
ORDER BY n.nspname, c.relname, l.pgllabel`

// reflectLabels returns each element table's labels, ordered by label name.
func reflectLabels(ctx context.Context, db exec.Querier, graphID uint32) (map[string][]schema.LabelProperties, error) {
	rows, err := db.Query(ctx, labelsSQL, graphID)
	if err != nil {
		return nil, fmt.Errorf("conform: read graph labels: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]schema.LabelProperties)
	for rows.Next() {
		var schemaName, table string
		var lp schema.LabelProperties
		if err := rows.Scan(&schemaName, &table, &lp.Label, &lp.Properties); err != nil {
			return nil, fmt.Errorf("conform: read graph labels: %w", err)
		}
		// The key is built here rather than concatenated in SQL, so it is
		// exactly what schema.QualifiedKey produces. A key assembled by the
		// database would have to encode the separator the same way, and the two
		// would silently stop matching the moment the encoding changed.
		out[schema.QualifiedKey(schemaName, table)] = append(out[schema.QualifiedKey(schemaName, table)], lp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conform: read graph labels: %w", err)
	}
	return out, nil
}

// elementsSQL lists the graph's elements with the edge wiring for each.
//
// The key columns are stored as attribute numbers in the element table, so each
// is resolved back to a column name through pg_attribute. pgekey (the element
// key) is deliberately not read: it is the surrogate id in every graph gopgql
// generates, [schema.Schema] has nowhere to put it, and Check does not compare
// it.
const elementsSQL = `
SELECT n.nspname::text                 AS schema_name,
       c.relname::text                 AS table_name,
       e.pgekind::text                 AS kind,
       COALESCE(sn.nspname::text, '')  AS source_schema,
       COALESCE(sc.relname::text, '')  AS source_table,
       COALESCE(dn.nspname::text, '')  AS dest_schema,
       COALESCE(dc.relname::text, '')  AS dest_table,
       COALESCE(sk.cols, '{}'::text[]) AS source_key,
       COALESCE(sr.cols, '{}'::text[]) AS source_ref,
       COALESCE(dk.cols, '{}'::text[]) AS dest_key,
       COALESCE(dr.cols, '{}'::text[]) AS dest_ref
FROM pg_catalog.pg_propgraph_element e
JOIN pg_catalog.pg_class c ON c.oid = e.pgerelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_propgraph_element sv ON sv.oid = e.pgesrcvertexid
LEFT JOIN pg_catalog.pg_class sc ON sc.oid = sv.pgerelid
LEFT JOIN pg_catalog.pg_namespace sn ON sn.oid = sc.relnamespace
LEFT JOIN pg_catalog.pg_propgraph_element dv ON dv.oid = e.pgedestvertexid
LEFT JOIN pg_catalog.pg_class dc ON dc.oid = dv.pgerelid
LEFT JOIN pg_catalog.pg_namespace dn ON dn.oid = dc.relnamespace
LEFT JOIN LATERAL (
    SELECT array_agg(a.attname::text ORDER BY u.ord) AS cols
    FROM unnest(e.pgesrckey) WITH ORDINALITY AS u(attnum, ord)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = e.pgerelid AND a.attnum = u.attnum
) sk ON true
LEFT JOIN LATERAL (
    SELECT array_agg(a.attname::text ORDER BY u.ord) AS cols
    FROM unnest(e.pgesrcref) WITH ORDINALITY AS u(attnum, ord)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = sv.pgerelid AND a.attnum = u.attnum
) sr ON true
LEFT JOIN LATERAL (
    SELECT array_agg(a.attname::text ORDER BY u.ord) AS cols
    FROM unnest(e.pgedestkey) WITH ORDINALITY AS u(attnum, ord)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = e.pgerelid AND a.attnum = u.attnum
) dk ON true
LEFT JOIN LATERAL (
    SELECT array_agg(a.attname::text ORDER BY u.ord) AS cols
    FROM unnest(e.pgedestref) WITH ORDINALITY AS u(attnum, ord)
    JOIN pg_catalog.pg_attribute a ON a.attrelid = dv.pgerelid AND a.attnum = u.attnum
) dr ON true
WHERE e.pgepgid = $1
ORDER BY n.nspname, c.relname`

// reflectedElement is one row of elementsSQL.
type reflectedElement struct {
	schemaName   string
	table        string
	kind         string
	sourceSchema string
	sourceTable  string
	destSchema   string
	destTable    string
	sourceKey    []string
	sourceRef    []string
	destKey      []string
	destRef      []string
}

// key is the qualified name the labels map holds this element under.
func (e reflectedElement) key() string { return schema.QualifiedKey(e.schemaName, e.table) }

func reflectElements(ctx context.Context, db exec.Querier, graphID uint32) ([]reflectedElement, error) {
	rows, err := db.Query(ctx, elementsSQL, graphID)
	if err != nil {
		return nil, fmt.Errorf("conform: read graph elements: %w", err)
	}
	defer rows.Close()

	var out []reflectedElement
	for rows.Next() {
		var el reflectedElement
		if err := rows.Scan(&el.schemaName, &el.table, &el.kind,
			&el.sourceSchema, &el.sourceTable, &el.destSchema, &el.destTable,
			&el.sourceKey, &el.sourceRef, &el.destKey, &el.destRef); err != nil {
			return nil, fmt.Errorf("conform: read graph elements: %w", err)
		}
		out = append(out, el)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conform: read graph elements: %w", err)
	}
	return out, nil
}
