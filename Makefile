.PHONY: build vet test lint vuln wasm docs release-check release-snapshot all

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

# Build the docs site. npm's prebuild hook stages the WASM module first, so the
# site can never be built against a stale one.
docs:
	cd docs && npm ci && npm run build

# Validate .goreleaser.yaml (schema + templates) without building anything.
# Requires: go install github.com/goreleaser/goreleaser/v2@latest
release-check:
	goreleaser check

# Full local release build into dist/ with nothing published. Skips docker too,
# so it needs neither a daemon nor buildx/QEMU for the arm64 image.
#
# GITLAB_TOKEN/GITEA_TOKEN are cleared because goreleaser refuses to start when
# more than one forge token is in the environment, and a dev machine may well
# have them for unrelated projects. This target publishes nothing, so dropping
# them changes no behaviour — releases go through GITHUB_TOKEN in CI.
release-snapshot:
	env -u GITLAB_TOKEN -u GITEA_TOKEN \
		goreleaser release --snapshot --clean --skip=publish,docker,announce

# release-* are deliberately not part of `all`: they need goreleaser installed.
all: build vet lint test
