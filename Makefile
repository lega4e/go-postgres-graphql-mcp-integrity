.PHONY: build vet test lint vuln wasm docs all

# Host build of all packages (cmd/wasm builds its host stub here).
build:
	go build ./...

vet:
	go vet ./...

# Integration tests. Requires Docker; boots postgres:19beta2 via testcontainers.
# The container-backed suite always runs — there is no skip path (SPEC.md §10).
test:
	go test -v -timeout 20m ./...

lint:
	golangci-lint run ./...

# Requires: go install golang.org/x/vuln/cmd/govulncheck@latest
vuln:
	govulncheck ./...

# Build the WASM playground module + wasm_exec.js into docs/public.
wasm:
	bash scripts/build-wasm.sh

# Build the docs site (implies wasm).
docs: wasm
	cd docs && npm ci && npm run build

all: build vet lint test
