//go:build js && wasm

// Command wasm exposes the gopgql pipeline to the docs playground.
//
// It is compiled with GOOS=js GOARCH=wasm and loaded by the docs site. The
// playground calls the real compiled Go — sdl, generator, migrate and compiler
// — with no JavaScript re-implementation (SPEC.md §7 → M1/M2 demo criteria).
//
// Two functions are exported. gopgqlGenerate takes an SDL string and a GraphQL
// query and returns {"migration", "sql", "error"} (the M1 demo). gopgqlDelta
// takes two SDL revisions and a query and returns {"init", "delta", "sql",
// "error"} — the M2 demo showing the delta migration folded and diffed between
// the revisions.
package main

import (
	"syscall/js"

	"github.com/lega4e/gopgql/playground"
)

func main() {
	js.Global().Set("gopgqlGenerate", js.FuncOf(generate))
	js.Global().Set("gopgqlDelta", js.FuncOf(delta))
	js.Global().Set("gopgqlNested", js.FuncOf(nested))
	js.Global().Set("gopgqlExampleSDL", js.ValueOf(playground.ExampleSDL))
	js.Global().Set("gopgqlExampleQuery", js.ValueOf(playground.ExampleQuery))
	js.Global().Set("gopgqlRevisedExampleSDL", js.ValueOf(playground.RevisedExampleSDL))
	js.Global().Set("gopgqlRevisedExampleQuery", js.ValueOf(playground.RevisedExampleQuery))
	js.Global().Set("gopgqlNestedExampleQuery", js.ValueOf(playground.NestedExampleQuery))
	js.Global().Set("gopgqlNestedExampleVarName", js.ValueOf(playground.NestedExampleVarName))
	js.Global().Set("gopgqlNestedExampleVarValue", js.ValueOf(playground.NestedExampleVarValue))
	// Signal to the page that the WASM module is ready.
	if cb := js.Global().Get("onGopgqlReady"); cb.Type() == js.TypeFunction {
		cb.Invoke()
	}
	// Block forever so the exported functions stay callable.
	select {}
}

// generate is the js.Func bound to window.gopgqlGenerate. It expects two string
// arguments: the SDL document and the GraphQL query.
func generate(_ js.Value, args []js.Value) any {
	result := map[string]any{"migration": "", "sql": "", "error": ""}
	if len(args) < 2 || args[0].Type() != js.TypeString || args[1].Type() != js.TypeString {
		result["error"] = "gopgqlGenerate expects (sdl, query) string arguments"
		return js.ValueOf(result)
	}
	out, err := playground.Run(args[0].String(), args[1].String())
	if err != nil {
		result["error"] = err.Error()
		return js.ValueOf(result)
	}
	result["migration"] = out.Migration
	result["sql"] = out.SQL
	return js.ValueOf(result)
}

// delta is the js.Func bound to window.gopgqlDelta. It expects three string
// arguments: the prior SDL, the revised SDL, and a GraphQL query against the
// revised SDL.
func delta(_ js.Value, args []js.Value) any {
	result := map[string]any{"init": "", "delta": "", "sql": "", "error": ""}
	if len(args) < 3 || args[0].Type() != js.TypeString ||
		args[1].Type() != js.TypeString || args[2].Type() != js.TypeString {
		result["error"] = "gopgqlDelta expects (oldSDL, newSDL, query) string arguments"
		return js.ValueOf(result)
	}
	out, err := playground.RunDelta(args[0].String(), args[1].String(), args[2].String())
	if err != nil {
		result["error"] = err.Error()
		return js.ValueOf(result)
	}
	result["init"] = out.Init
	result["delta"] = out.Delta
	result["sql"] = out.SQL
	return js.ValueOf(result)
}

// nested is the js.Func bound to window.gopgqlNested. It expects three string
// arguments: the SDL document, the nested GraphQL query, and the value bound to
// the query's variable. It returns {"sql", "params", "json", "error"} — the M3
// demo showing the ordered $n placeholder and the shaped nested JSON.
func nested(_ js.Value, args []js.Value) any {
	result := map[string]any{"sql": "", "params": "", "json": "", "error": ""}
	if len(args) < 3 || args[0].Type() != js.TypeString ||
		args[1].Type() != js.TypeString || args[2].Type() != js.TypeString {
		result["error"] = "gopgqlNested expects (sdl, query, varValue) string arguments"
		return js.ValueOf(result)
	}
	vars := map[string]any{playground.NestedExampleVarName: args[2].String()}
	out, err := playground.RunNested(args[0].String(), args[1].String(), vars)
	if err != nil {
		result["error"] = err.Error()
		return js.ValueOf(result)
	}
	result["sql"] = out.SQL
	result["params"] = out.Params
	result["json"] = out.JSON
	return js.ValueOf(result)
}
