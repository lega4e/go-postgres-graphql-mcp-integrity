//go:build !(js && wasm)

// This file provides a host-platform build of cmd/wasm so that `go build ./...`,
// `go vet ./...`, and the linter work on any platform. The real command targets
// WebAssembly only (see main.go); building it for the host is not meaningful.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr,
		"cmd/wasm is a WebAssembly build target; build it with: "+
			"GOOS=js GOARCH=wasm go build -o gopgql.wasm ./cmd/wasm")
	os.Exit(1)
}
