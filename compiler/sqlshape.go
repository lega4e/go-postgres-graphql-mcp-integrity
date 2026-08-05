package compiler

import (
	"fmt"
	"strconv"
	"strings"
)

// renderShaped assembles the SQL-side strategy's statement: one row of one
// column named `response`, holding the whole nested GraphQL response as JSON
// text the database itself assembled (SPEC.md §7 → M8, design D2).
//
// The MATCH patterns, their predicates and their bind parameters are byte-for-
// byte what the flat query emits — the same fragments, rendered by the same
// graphTable. Only the projection around them differs, which is why "which rows
// match" is not part of what byte-identity has to prove.
//
// The shape of the statement is:
//
//	WITH q0 AS (SELECT … FROM GRAPH_TABLE (…)), q1 AS (…)
//	SELECT (json_build_object('persons', COALESCE(json_agg(
//	          json_build_object('name', v0_c0, 'follows', v1_a)
//	          ORDER BY v0_k), '[]'::json)))::text AS response
//	FROM ( … one subquery per projection level, aggregated bottom-up … ) AS r
//
// Three details of that rendering are load-bearing:
//
//   - **json, never jsonb.** jsonb_build_object sorts keys by length-then-bytes
//     and drops duplicates, and costs a parse-and-reserialise for a value whose
//     only destination is text. json does none of that.
//   - **Every aggregate is COALESCEd.** json_agg over an empty set returns SQL
//     NULL where the Go-side shaper returns an empty list, so without it a root
//     field matching nothing is {"persons":null} on one side and
//     {"persons":[]} on the other.
//   - **Each fragment is aggregated to a JSON array before the join.** Where M5
//     split a branching level into one GRAPH_TABLE per relationship, the flat
//     query LEFT JOINs them and a parent with m and n children yields m×n rows.
//     Here each branch is aggregated to a single array first, so that
//     cross-product never forms — the clearest reason SQL-side shaping can win.
func (b *builder) renderShaped(graphName string, root *Selection) string {
	var sb strings.Builder

	sb.WriteString("WITH ")
	for i, f := range b.frags {
		if i > 0 {
			sb.WriteString(",\n")
		}
		fmt.Fprintf(&sb, "%s AS (\n  SELECT %s\n  FROM %s\n)", f.name,
			strings.Join(f.cteColumns(), ", "), f.graphTable(graphName, "  "))
	}

	// The root's key is projected by an inner join inside GRAPH_TABLE, so it is
	// never NULL and needs no FILTER; the COALESCE is still required, for the
	// query that matches nothing at all.
	rel := b.levelRel(root, nil)
	src := b.nextAlias()
	fmt.Fprintf(&sb, "\nSELECT (json_build_object(%s, COALESCE(json_agg(%s ORDER BY %s), '[]'::json)))::text AS response\nFROM %s AS %s",
		quoteLiteral(root.ResponseKey), b.objectOf(src, root),
		strings.Join(qualify(src, root.KeyColumns), ", "),
		rel, src)
	return sb.String()
}

// cteColumns renders the column list of a fragment's CTE: everything the outer
// query can see of it, plus the branch-point identity a branch fragment joins on.
func (f *fragment) cteColumns() []string {
	return append(selectList(f.outs, ""), f.joinKeys...)
}

// levelRel renders the subquery that produces one row per distinct path down to
// sel: the ancestor keys, sel's own key and scalar columns, and one aggregated
// JSON array column per nested relationship.
//
// anc is the ancestor key columns in scope, outermost first. It is what keeps a
// child's aggregate attached to the right parent — grouping a child level by the
// full ancestor path rather than by its immediate parent alone, so two parents
// that share a child do not pool it.
//
// The base relation is SELECT DISTINCT rather than a bare SELECT because a
// GRAPH_TABLE row is a *path*, not a vertex: a parent with N children appears on
// N rows. The Go-side shaper collapses those when it buckets rows by each level's
// key — "no duplicate parents across the one-to-many fan-out", which is what M3
// established — and this is where the SQL side does the same. Without it a
// parent would be aggregated once per child.
//
// It is not needed for parallel edges: every generated edge table carries
// PRIMARY KEY (source_id, target_id) (generator.go), so two edges between the
// same pair cannot exist to produce a duplicate path in the first place.
func (b *builder) levelRel(sel *Selection, anc []string) string {
	group := append(append([]string{}, anc...), sel.KeyColumns...)

	own := append([]string{}, group...)
	for _, f := range sel.Fields {
		own = append(own, f.Column)
	}

	base := b.nextAlias()

	var sb strings.Builder
	sb.WriteString("(SELECT ")
	sb.WriteString(strings.Join(qualify(base, own), ", "))

	// Each child's aggregate is joined in, so its alias has to be known before
	// the select list is closed.
	childAlias := make([]string, len(sel.Children))
	for i, child := range sel.Children {
		childAlias[i] = b.nextAlias()
		// A parent that matched no child on this branch has no row in the
		// child's aggregate at all, so this COALESCE is what turns the missing
		// join partner into an empty list rather than a null.
		fmt.Fprintf(&sb, ", COALESCE(%s.%s, '[]'::json) AS %s",
			childAlias[i], aggColumn(child), aggColumn(child))
	}

	fmt.Fprintf(&sb, "\nFROM (SELECT DISTINCT %s FROM %s) AS %s",
		strings.Join(own, ", "), flatSource(sel.frag), base)
	for i, child := range sel.Children {
		fmt.Fprintf(&sb, "\nLEFT JOIN %s AS %s ON %s",
			b.childAggregate(child, group), childAlias[i], joinOn(childAlias[i], base, group))
	}
	sb.WriteString(")")
	return sb.String()
}

// childAggregate renders the subquery that collapses one nested relationship
// into a single JSON array per parent: the level's own relation, grouped back up
// to its parent's key path.
//
// The ORDER BY inside json_agg is the level's key — the same key, in the same
// direction, that the flat query's ORDER BY carries — which is what makes the
// two strategies' lists agree element for element (design D4).
//
// The FILTER is for the branch case: a LEFT JOINed branch that matched nothing
// contributes a row whose key is NULL, and without the filter that row would
// aggregate to [null] instead of [].
//
// With a multi-column identity the test is "not *every* component is NULL", not
// "no component is NULL". A branch that matched nothing has all of them NULL; a
// row with only some of them NULL is a real row whose key is partly unknown,
// and a @key column is UNIQUE but never NOT NULL, so it can happen. Requiring
// all of them to be present would silently drop such a row — the same mistake
// the isomorphism guard avoids with IS DISTINCT FROM. shape's Go-side grouping
// applies exactly this rule, which is what keeps the two strategies in step.
func (b *builder) childAggregate(child *Selection, group []string) string {
	rel := b.levelRel(child, group)
	src := b.nextAlias()
	grouped := qualify(src, group)
	return fmt.Sprintf(
		"(SELECT %s, COALESCE(json_agg(%s ORDER BY %s) FILTER (WHERE %s), '[]'::json) AS %s\nFROM %s AS %s\nGROUP BY %s)",
		strings.Join(grouped, ", "),
		b.objectOf(src, child), strings.Join(qualify(src, child.KeyColumns), ", "),
		presentPredicate(src, child.KeyColumns), aggColumn(child),
		rel, src,
		strings.Join(grouped, ", "))
}

// objectOf renders one response object for a level: its scalar fields under
// their GraphQL response keys, then its nested relationships under theirs.
//
// The key *order* here is immaterial to byte-identity — the response never
// reaches a caller as PostgreSQL wrote it, and Go's map-key sort decides the
// order on both paths (design D3) — so selection order is used, because it is
// the order a reader of the query expects.
func (b *builder) objectOf(src string, sel *Selection) string {
	args := make([]string, 0, 2*(len(sel.Fields)+len(sel.Children)))
	for _, f := range sel.Fields {
		args = append(args, quoteLiteral(f.ResponseKey), src+"."+f.Column)
	}
	for _, child := range sel.Children {
		args = append(args, quoteLiteral(child.ResponseKey), src+"."+aggColumn(child))
	}
	if len(args) == 0 {
		return "json_build_object()"
	}
	return "json_build_object(" + strings.Join(args, ", ") + ")"
}

// flatSource renders the relation a fragment's rows are read from: its own CTE
// for the spine, and for a branch the chain of LEFT JOINs from the spine down to
// it, so that every ancestor key is in scope.
//
// The join carries the same isomorphism guards the flat query puts in its ON
// clause. They have to travel with the join rather than into the aggregate,
// because a guard spanning the split compares a branch vertex against an
// ancestor that only exists on the other side of it (SPEC.md §2.2).
func flatSource(f *fragment) string {
	var chain []*fragment
	for x := f; x != nil; x = x.parent {
		chain = append([]*fragment{x}, chain...)
	}

	var sb strings.Builder
	sb.WriteString(chain[0].name)
	for _, g := range chain[1:] {
		fmt.Fprintf(&sb, " LEFT JOIN %s ON %s", g.name, strings.Join(branchJoinOn(g), " AND "))
	}
	return sb.String()
}

// presentPredicate renders "this row exists": at least one identity component is
// non-NULL. See childAggregate for why it is not "every component".
func presentPredicate(alias string, cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("%s.%s IS NOT NULL", alias, c)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// joinOn renders the equality that reattaches a child's aggregate to its parent
// row, over the full ancestor key path.
//
// Plain `=` is enough even though a branch level's key can be NULL: a NULL key
// means the branch matched nothing, and such a row is excluded from its parent's
// aggregate by the FILTER before the join is ever consulted.
func joinOn(child, base string, group []string) string {
	preds := make([]string, len(group))
	for i, c := range group {
		preds[i] = fmt.Sprintf("%s.%s = %s.%s", child, c, base, c)
	}
	return strings.Join(preds, " AND ")
}

// qualify prefixes each column with a derived-table alias. The alias is dropped
// from the emitted output-column name, so `r1.v0_k` is still exposed as `v0_k`.
func qualify(alias string, cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = alias + "." + c
	}
	return out
}

// aggColumn is the column name a level's aggregated JSON array is carried under.
// It is derived from the level's vertex alias, which is unique across every
// fragment of the query, so no two levels can collide.
func aggColumn(sel *Selection) string { return sel.Alias + "_a" }

// nextAlias returns a fresh alias for a derived table. PostgreSQL requires one
// on every subquery in a FROM clause, and they are never referred to by name —
// every column in this statement is unique across the whole query.
func (b *builder) nextAlias() string {
	b.relN++
	return "r" + strconv.Itoa(b.relN)
}

// quoteLiteral renders a GraphQL response key as a SQL string literal. A key is
// a GraphQL name — letters, digits and underscore — so it cannot contain a
// quote; doubling any anyway costs nothing and keeps the emission safe if the
// language's name rules ever widen.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
