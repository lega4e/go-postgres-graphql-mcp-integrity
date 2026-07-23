//go:build js && wasm

// Command wasm exposes the gopgql M0 preview generator to JavaScript.
//
// It is compiled with GOOS=js GOARCH=wasm and loaded by the docs playground.
// The playground calls the real compiled Go — there is no JavaScript
// re-implementation of the translation. The single exported function,
// gopgqlGenerate, takes an SDL string and returns a plain JS object with
// "ddl", "query" and "error" fields.
package main

import (
	"syscall/js"

	"github.com/lega4e/gopgql/demo"
)

func main() {
	js.Global().Set("gopgqlGenerate", js.FuncOf(generate))
	js.Global().Set("gopgqlExampleSDL", js.ValueOf(demo.ExampleSDL))
	// Signal to the page that the WASM module is ready.
	if cb := js.Global().Get("onGopgqlReady"); cb.Type() == js.TypeFunction {
		cb.Invoke()
	}
	// Block forever so the exported functions stay callable.
	select {}
}

// generate is the js.Func bound to window.gopgqlGenerate.
func generate(_ js.Value, args []js.Value) any {
	result := map[string]any{"ddl": "", "query": "", "error": ""}
	if len(args) < 1 || args[0].Type() != js.TypeString {
		result["error"] = "gopgqlGenerate expects a single SDL string argument"
		return js.ValueOf(result)
	}
	out, err := demo.Generate(args[0].String())
	if err != nil {
		result["error"] = err.Error()
		return js.ValueOf(result)
	}
	result["ddl"] = out.DDL
	result["query"] = out.Query
	return js.ValueOf(result)
}
