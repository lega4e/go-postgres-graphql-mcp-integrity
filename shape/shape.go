// Package shape regroups the flat rows returned by a compiled query into the
// nested GraphQL response.
//
// A compiled query's MATCH pattern fans out: a parent that matches N children
// yields N flat rows. shape walks the projection tree (SPEC.md §7 → M3),
// grouping rows by each level's hidden key column so every parent appears exactly
// once with its children nested beneath it — no duplicate parents across the
// one-to-many fan-out. First-seen order is preserved at every level.
//
// This is the Go-side strategy; an SQL-side json_agg strategy is added and
// benchmarked in M8 (SPEC.md §3, decision 4).
package shape

import (
	"fmt"

	"github.com/lega4e/gopgql/compiler"
)

// Rows regroups flat result rows into the nested GraphQL response. Each element
// of rows maps a projected column name (a field's Column, or a level's
// KeyColumn) to its scanned value. The returned map has one entry — the root
// field's response key — whose value is the ordered, deduplicated list of parent
// objects.
func Rows(proj compiler.Projection, rows []map[string]any) map[string]any {
	root := proj.Root
	return map[string]any{root.ResponseKey: group(root, rows)}
}

// group buckets rows by sel.KeyColumn and builds one object per distinct key,
// recursing into nested relationships with just that parent's rows. Rows whose
// key at this level is null are skipped: an inner-join MATCH never produces a
// null key, but a defensive skip keeps a stray null from becoming a phantom
// parent.
func group(sel *compiler.Selection, rows []map[string]any) []any {
	var order []string
	buckets := map[string][]map[string]any{}
	for _, row := range rows {
		kv, ok := row[sel.KeyColumn]
		if !ok || kv == nil {
			continue
		}
		k := keyString(kv)
		if _, seen := buckets[k]; !seen {
			order = append(order, k)
		}
		buckets[k] = append(buckets[k], row)
	}

	list := make([]any, 0, len(order))
	for _, k := range order {
		grp := buckets[k]
		obj := make(map[string]any, len(sel.Fields)+len(sel.Children))
		for _, f := range sel.Fields {
			obj[f.ResponseKey] = grp[0][f.Column]
		}
		for _, child := range sel.Children {
			obj[child.ResponseKey] = group(child, grp)
		}
		list = append(list, obj)
	}
	return list
}

// keyString renders a key value as a stable string usable as a map key. Scanned
// identifiers (uuids in particular) may decode to non-comparable types such as
// byte slices, which cannot be map keys directly; formatting sidesteps that
// while grouping equal ids together.
func keyString(v any) string {
	return fmt.Sprintf("%v", v)
}
