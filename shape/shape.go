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
	"strings"

	"github.com/lega4e/gopgql/compiler"
)

// Rows regroups flat result rows into the nested GraphQL response. Each element
// of rows maps a projected column name (a field's Column, or a level's
// KeyColumn) to its scanned value. The returned map has one entry — the root
// field's response key — whose value is the ordered, deduplicated list of parent
// objects.
//
// Every leaf is normalised on the way through, so the response this builds is
// the same canonical value shape.Decode builds from the SQL-side strategy's JSON
// and Encode writes identical bytes for either (design D3, D5). Normalisation is
// what makes the error return necessary: a non-finite Float has no JSON form,
// and it fails here rather than succeeding on one path and failing on the other.
func Rows(proj compiler.Projection, rows []map[string]any) (map[string]any, error) {
	root := proj.Root
	list, err := group(root, rows)
	if err != nil {
		return nil, err
	}
	return map[string]any{root.ResponseKey: list}, nil
}

// group buckets rows by sel.KeyColumns and builds one object per distinct key,
// recursing into nested relationships with just that parent's rows.
//
// A row whose identity is *wholly* null is skipped: that is what a LEFT JOINed
// branch which matched nothing looks like, and without the skip it would become
// a phantom parent. A row with only some components null is a real row whose key
// is partly unknown — a @key column is UNIQUE but never NOT NULL — and is kept.
// The SQL-side strategy's FILTER applies the same rule, which is what keeps the
// two agreeing (design D4).
func group(sel *compiler.Selection, rows []map[string]any) ([]any, error) {
	var order []string
	buckets := map[string][]map[string]any{}
	for _, row := range rows {
		k, present := keyOf(sel.KeyColumns, row)
		if !present {
			continue
		}
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
			val, err := normalise(f, grp[0][f.Column])
			if err != nil {
				return nil, err
			}
			obj[f.ResponseKey] = val
		}
		for _, child := range sel.Children {
			nested, err := group(child, grp)
			if err != nil {
				return nil, err
			}
			obj[child.ResponseKey] = nested
		}
		list = append(list, obj)
	}
	return list, nil
}

// keyOf renders a row's identity as a stable map key, and reports whether the
// row has an identity at all.
//
// Scanned identifiers — uuids in particular — may decode to non-comparable Go
// types such as byte slices, which cannot be map keys directly; formatting
// sidesteps that while still grouping equal values together.
//
// The components are joined with **NUL**, and that is load-bearing rather than
// cosmetic. A dedup collision is silent: two rows that are not the same row
// merge, one of them disappears from the response, and nothing reports it. With
// a printable separator, ("a b", "c") and ("a", "b c") encode identically the
// moment a key column holds a space — and text keys are exactly what a natural
// key tends to be. NUL cannot occur in a PostgreSQL text value at all, so the
// encoding is unambiguous by construction. It is the same discipline
// schema.QualifiedKey uses, for the same reason.
func keyOf(cols []string, row map[string]any) (string, bool) {
	parts := make([]string, len(cols))
	present := false
	for i, c := range cols {
		v, ok := row[c]
		if ok && v != nil {
			present = true
		}
		parts[i] = fmt.Sprintf("%v", v)
	}
	return strings.Join(parts, "\x00"), present
}
