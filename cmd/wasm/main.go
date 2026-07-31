//go:build js && wasm

// Command wasm exposes the gopgql pipeline to the docs playground.
//
// It is compiled with GOOS=js GOARCH=wasm and loaded by the docs site. The
// playground calls the real compiled Go — sdl, generator, migrate and compiler
// — with no JavaScript re-implementation and no database. Every value it
// returns is generated from the editable inputs (SDL, query, variables); it
// never fabricates query results, which would require rows from PostgreSQL
// (SPEC.md §4).
//
// Five functions are exported:
//
//   - gopgqlSchema(sdl) -> {schema, error}
//   - gopgqlGraph(sdl) -> {graph, error}
//   - gopgqlMigration(sdl) -> {migration, error}
//   - gopgqlCompile(sdl, query, varsJSON[, maxDepth]) -> {sql, params, args, error, depthExceeded, maxDepth}
//   - gopgqlDelta(oldSDL, newSDL) -> {delta, changed, error}
//
// gopgqlCompile classifies a depth rejection separately from other errors so the
// page can show it as the designed outcome it is: SQL/PGQ has no
// variable-length paths, so a selection past MaxDepth is refused at compile
// time rather than silently truncated (SPEC.md §3, decision 3).
//
// There is deliberately no gopgqlConform. The conformance check reads a live
// database, so the conform package sits on the pgx side of the WASM boundary
// and nothing here may import it (SPEC.md §4.1, design D5). The page shows the
// half of the comparison that is computable in a browser — the graph mapping
// the SDL describes, via gopgqlGraph — beside a recorded report, and says which
// is which.
package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/lega4e/gopgql/playground"
)

// apiVersion is the shape of the surface exported below. The page checks it
// before calling anything.
//
// docs/public/gopgql.wasm is a build artefact that is never committed
// (SPEC.md §8.3), and it is served from a stable, unhashed URL — so it is easy
// to end up running a new page against an old module, from a stale working
// copy or a browser cache. An older module silently ignoring an argument the
// page now passes is the worst kind of failure to debug: the control moves and
// nothing happens. Bump this whenever an exported function's arguments or
// result shape change — and whenever the page starts depending on a new export,
// so a panel cannot come up blank against a module that lacks it.
//
// Bump it in the **same commit** as REQUIRED_API_VERSION in docs/src/main.js.
// Splitting the two leaves every build in between a mismatched pair that the
// page refuses to run — invisible to CI, which never loads the page, and
// immediately visible to anyone who opens a PR preview. TestAPIVersionsAgree
// enforces this.
const apiVersion = 6

func main() {
	js.Global().Set("gopgqlApiVersion", js.ValueOf(apiVersion))
	js.Global().Set("gopgqlSchema", js.FuncOf(schemaDDL))
	js.Global().Set("gopgqlGraph", js.FuncOf(graphDDL))
	js.Global().Set("gopgqlMigration", js.FuncOf(migration))
	js.Global().Set("gopgqlCompile", js.FuncOf(compile))
	js.Global().Set("gopgqlDelta", js.FuncOf(delta))
	js.Global().Set("gopgqlExampleSDL", js.ValueOf(playground.ExampleSDL))
	js.Global().Set("gopgqlExampleQuery", js.ValueOf(playground.ExampleQuery))
	js.Global().Set("gopgqlExampleVars", js.ValueOf(playground.ExampleVars))
	js.Global().Set("gopgqlExampleSeed", js.ValueOf(playground.ExampleSeed))
	js.Global().Set("gopgqlRevisedExampleSDL", js.ValueOf(playground.RevisedExampleSDL))
	js.Global().Set("gopgqlExampleDeepQuery", js.ValueOf(playground.ExampleDeepQuery))
	js.Global().Set("gopgqlExampleMultiQuery", js.ValueOf(playground.ExampleMultiPatternQuery))
	js.Global().Set("gopgqlExampleDirectivesSDL", js.ValueOf(playground.ExampleDirectivesSDL))
	js.Global().Set("gopgqlExampleDirectivesQuery", js.ValueOf(playground.ExampleDirectivesQuery))
	js.Global().Set("gopgqlExampleDirectivesVars", js.ValueOf(playground.ExampleDirectivesVars))
	js.Global().Set("gopgqlExampleDirectivesSeed", js.ValueOf(playground.ExampleDirectivesSeed))
	js.Global().Set("gopgqlExampleInterfaceSDL", js.ValueOf(playground.ExampleInterfaceSDL))
	js.Global().Set("gopgqlExampleInterfaceQuery", js.ValueOf(playground.ExampleInterfaceQuery))
	js.Global().Set("gopgqlExampleInterfaceSeed", js.ValueOf(playground.ExampleInterfaceSeed))
	js.Global().Set("gopgqlExampleConstraintsSDL", js.ValueOf(playground.ExampleConstraintsSDL))
	js.Global().Set("gopgqlExampleConstraintsQuery", js.ValueOf(playground.ExampleConstraintsQuery))
	js.Global().Set("gopgqlExampleConstraintsVars", js.ValueOf(playground.ExampleConstraintsVars))
	js.Global().Set("gopgqlExampleConstraintsSeed", js.ValueOf(playground.ExampleConstraintsSeed))
	js.Global().Set("gopgqlRevisedConstraintsSDL", js.ValueOf(playground.RevisedConstraintsSDL))
	// A fixture, not a result: see playground.ExampleConformanceReport and the
	// note on the page. It is exported alongside the generated values so the
	// page has one source for its content, not so it can pass as one of them.
	js.Global().Set("gopgqlConformanceReport", js.ValueOf(playground.ExampleConformanceReport))
	js.Global().Set("gopgqlMaxDepth", js.ValueOf(playground.MaxDepth()))
	// Signal to the page that the WASM module is ready.
	if cb := js.Global().Get("onGopgqlReady"); cb.Type() == js.TypeFunction {
		cb.Invoke()
	}
	// Block forever so the exported functions stay callable.
	select {}
}

// schemaDDL is bound to window.gopgqlSchema. It expects one string argument:
// the SDL document. It returns the DDL the schema generates — the tables and
// property graph a compiled query runs against.
func schemaDDL(_ js.Value, args []js.Value) any {
	result := map[string]any{"schema": "", "error": ""}
	if len(args) < 1 || args[0].Type() != js.TypeString {
		result["error"] = "gopgqlSchema expects (sdl) a string argument"
		return js.ValueOf(result)
	}
	out, err := playground.Schema(args[0].String())
	if err != nil {
		result["error"] = err.Error()
		return js.ValueOf(result)
	}
	result["schema"] = out
	return js.ValueOf(result)
}

// graphDDL is bound to window.gopgqlGraph. It expects one string argument: the
// SDL document. It returns only the CREATE PROPERTY GRAPH statement — the
// elements, labels and properties, which is the entire surface a conformance
// check compares, and therefore the honest way to show what that check is
// about.
func graphDDL(_ js.Value, args []js.Value) any {
	result := map[string]any{"graph": "", "error": ""}
	if len(args) < 1 || args[0].Type() != js.TypeString {
		result["error"] = "gopgqlGraph expects (sdl) a string argument"
		return js.ValueOf(result)
	}
	out, err := playground.GraphMapping(args[0].String())
	if err != nil {
		result["error"] = err.Error()
		return js.ValueOf(result)
	}
	result["graph"] = out
	return js.ValueOf(result)
}

// migration is bound to window.gopgqlMigration. It expects one string argument:
// the SDL document.
func migration(_ js.Value, args []js.Value) any {
	result := map[string]any{"migration": "", "error": ""}
	if len(args) < 1 || args[0].Type() != js.TypeString {
		result["error"] = "gopgqlMigration expects (sdl) a string argument"
		return js.ValueOf(result)
	}
	out, err := playground.Migration(args[0].String())
	if err != nil {
		result["error"] = err.Error()
		return js.ValueOf(result)
	}
	result["migration"] = out
	return js.ValueOf(result)
}

// compile is bound to window.gopgqlCompile. It expects three string arguments —
// the SDL, the GraphQL query, and a variables document as JSON (may be empty) —
// and an optional fourth: the traversal-depth ceiling, in hops from the root
// field. Omitting it uses the compiler's default. The returned maxDepth is the
// ceiling that was applied, so the page can always name the limit it is showing.
//
// It returns the bind values twice, for two different readers. `params` is the
// rendering a person reads ("$1 = Alice"); `args` is a JSON array the page
// decodes with JSON.parse and binds to a query it executes. JSON rather than
// js.ValueOf on the []any because the boundary is then explicit, is symmetric
// with how variables cross the other way (parseVars), and keeps this function
// clear of js.ValueOf's panic-on-unsupported-type behaviour as the compiler's
// value set grows.
//
// The precision limit is JavaScript's number model, not JSON's. A GraphQL Int
// literal is a Go int64 (compiler.value parses ast.IntValue with
// strconv.ParseInt), and *any* crossing into JS — JSON.parse or js.ValueOf
// alike — lands it in a float64, so an integer past 2^53 loses precision either
// way. Switching encodings would not change that. The example schemas use text
// and small integers, so it does not bite in practice.
func compile(_ js.Value, args []js.Value) any {
	maxDepth := playground.MaxDepth()
	if len(args) >= 4 && args[3].Type() == js.TypeNumber {
		maxDepth = args[3].Int()
	}
	result := map[string]any{
		"sql": "", "params": "", "args": "[]", "error": "",
		"depthExceeded": false, "maxDepth": maxDepth,
	}
	if len(args) < 3 || args[0].Type() != js.TypeString ||
		args[1].Type() != js.TypeString || args[2].Type() != js.TypeString {
		result["error"] = "gopgqlCompile expects (sdl, query, varsJSON) string arguments"
		return js.ValueOf(result)
	}
	vars, err := parseVars(args[2].String())
	if err != nil {
		result["error"] = "variables: " + err.Error()
		return js.ValueOf(result)
	}
	out, err := playground.CompileWithMaxDepth(args[0].String(), args[1].String(), vars, maxDepth)
	if err != nil {
		result["error"] = err.Error()
		if limit, ok := playground.DepthExceeded(err); ok {
			result["depthExceeded"] = true
			result["maxDepth"] = limit
		}
		return js.ValueOf(result)
	}
	// A query with no variables gets "[]" rather than json.Marshal's "null", so
	// the page always JSON.parses an array and never has to special-case it.
	if len(out.Args) > 0 {
		encoded, err := json.Marshal(out.Args)
		if err != nil {
			// Unreachable for the types the compiler produces today, but a
			// silently empty args is precisely the failure the API version bump
			// exists to prevent, so it is reported rather than dropped.
			result["error"] = "bind parameters: " + err.Error()
			return js.ValueOf(result)
		}
		result["args"] = string(encoded)
	}
	result["sql"] = out.SQL
	result["params"] = out.Params
	return js.ValueOf(result)
}

// delta is bound to window.gopgqlDelta. It expects two string arguments: the
// prior SDL and the revised SDL.
func delta(_ js.Value, args []js.Value) any {
	result := map[string]any{"delta": "", "changed": false, "error": ""}
	if len(args) < 2 || args[0].Type() != js.TypeString || args[1].Type() != js.TypeString {
		result["error"] = "gopgqlDelta expects (oldSDL, newSDL) string arguments"
		return js.ValueOf(result)
	}
	out, changed, err := playground.Delta(args[0].String(), args[1].String())
	if err != nil {
		result["error"] = err.Error()
		return js.ValueOf(result)
	}
	result["delta"] = out
	result["changed"] = changed
	return js.ValueOf(result)
}

// parseVars decodes a variables document. An empty or whitespace-only string is
// treated as "no variables".
func parseVars(s string) (map[string]any, error) {
	trimmed := ""
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			trimmed = s
			break
		}
	}
	if trimmed == "" {
		return nil, nil
	}
	var vars map[string]any
	if err := json.Unmarshal([]byte(s), &vars); err != nil {
		return nil, err
	}
	return vars, nil
}
