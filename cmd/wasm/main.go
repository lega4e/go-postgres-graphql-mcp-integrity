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
// Four functions are exported:
//
//   - gopgqlSchema(sdl) -> {schema, error}
//   - gopgqlMigration(sdl) -> {migration, error}
//   - gopgqlCompile(sdl, query, varsJSON) -> {sql, params, error, depthExceeded, maxDepth}
//   - gopgqlDelta(oldSDL, newSDL) -> {delta, changed, error}
//
// gopgqlCompile classifies a depth rejection separately from other errors so the
// page can show it as the designed outcome it is: SQL/PGQ has no
// variable-length paths, so a selection past MaxDepth is refused at compile
// time rather than silently truncated (SPEC.md §3, decision 3).
package main

import (
	"encoding/json"
	"syscall/js"

	"github.com/lega4e/gopgql/playground"
)

func main() {
	js.Global().Set("gopgqlSchema", js.FuncOf(schemaDDL))
	js.Global().Set("gopgqlMigration", js.FuncOf(migration))
	js.Global().Set("gopgqlCompile", js.FuncOf(compile))
	js.Global().Set("gopgqlDelta", js.FuncOf(delta))
	js.Global().Set("gopgqlExampleSDL", js.ValueOf(playground.ExampleSDL))
	js.Global().Set("gopgqlExampleQuery", js.ValueOf(playground.ExampleQuery))
	js.Global().Set("gopgqlExampleVars", js.ValueOf(playground.ExampleVars))
	js.Global().Set("gopgqlRevisedExampleSDL", js.ValueOf(playground.RevisedExampleSDL))
	js.Global().Set("gopgqlExampleDeepQuery", js.ValueOf(playground.ExampleDeepQuery))
	js.Global().Set("gopgqlExampleInterfaceSDL", js.ValueOf(playground.ExampleInterfaceSDL))
	js.Global().Set("gopgqlExampleInterfaceQuery", js.ValueOf(playground.ExampleInterfaceQuery))
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

// compile is bound to window.gopgqlCompile. It expects three string arguments:
// the SDL, the GraphQL query, and a variables document as JSON (may be empty).
func compile(_ js.Value, args []js.Value) any {
	result := map[string]any{
		"sql": "", "params": "", "error": "",
		"depthExceeded": false, "maxDepth": playground.MaxDepth(),
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
	out, err := playground.Compile(args[0].String(), args[1].String(), vars)
	if err != nil {
		result["error"] = err.Error()
		if limit, ok := playground.DepthExceeded(err); ok {
			result["depthExceeded"] = true
			result["maxDepth"] = limit
		}
		return js.ValueOf(result)
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
