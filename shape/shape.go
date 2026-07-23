// Package shape regroups the flat rows returned by a compiled query into the
// nested GraphQL response.
//
// M1 handles the single-root-field, no-nesting case: the rows become a JSON
// array under the root field's response key, each row an object keyed by the
// projected fields' response keys. Parent-row deduplication across one-to-many
// fan-out — needed once traversal lands — arrives with the shaping work in M3
// (SPEC.md §7). This is the Go-side strategy; an SQL-side json_agg strategy is
// added and benchmarked in M8.
package shape

import "github.com/lega4e/gopgql/compiler"

// Rows regroups flat result rows into the nested GraphQL response for a
// single-root-field query. Each element of rows maps a projected field's
// response key to its scanned value. The returned map has one entry — the root
// field's response key — whose value is the ordered list of row objects.
func Rows(proj compiler.Projection, rows []map[string]any) map[string]any {
	list := make([]any, 0, len(rows))
	for _, row := range rows {
		obj := make(map[string]any, len(proj.Fields))
		for _, f := range proj.Fields {
			obj[f.ResponseKey] = row[f.ResponseKey]
		}
		list = append(list, obj)
	}
	return map[string]any{proj.ResponseKey: list}
}
