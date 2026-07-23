//go:build js && wasm

// Command wasm exposes the gopgql pipeline to the docs playground.
//
// It is compiled with GOOS=js GOARCH=wasm and loaded by the docs site. The
// playground calls the real compiled Go — sdl, generator, migrate and compiler
// — with no JavaScript re-implementation (SPEC.md §7 → M1 demo criterion). The
// single exported function, gopgqlGenerate, takes an SDL string and a GraphQL
// query and returns a plain JS object with "migration", "sql" and "error"
// fields.
package main

import (
	"syscall/js"

	"github.com/lega4e/gopgql/playground"
)

func main() {
	js.Global().Set("gopgqlGenerate", js.FuncOf(generate))
	js.Global().Set("gopgqlExampleSDL", js.ValueOf(playground.ExampleSDL))
	js.Global().Set("gopgqlExampleQuery", js.ValueOf(playground.ExampleQuery))
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
