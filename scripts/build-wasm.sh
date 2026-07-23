#!/usr/bin/env bash
# Build the gopgql WASM playground module and stage it (with Go's wasm_exec.js
# runtime glue) into docs/public so `vite build` copies them to the site root.
#
# The .wasm binary and wasm_exec.js are build artefacts and are never committed
# (SPEC.md §8.3); they are produced fresh here for both local builds and CI.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="${repo_root}/docs/public"
mkdir -p "${out_dir}"

echo "building gopgql.wasm…"
GOOS=js GOARCH=wasm go build -trimpath -o "${out_dir}/gopgql.wasm" "${repo_root}/cmd/wasm"

goroot="$(go env GOROOT)"
exec_js=""
for candidate in "${goroot}/lib/wasm/wasm_exec.js" "${goroot}/misc/wasm/wasm_exec.js"; do
  if [[ -f "${candidate}" ]]; then
    exec_js="${candidate}"
    break
  fi
done
if [[ -z "${exec_js}" ]]; then
  echo "error: could not find wasm_exec.js under ${goroot}" >&2
  exit 1
fi
cp "${exec_js}" "${out_dir}/wasm_exec.js"

echo "staged $(basename "${out_dir}")/gopgql.wasm and wasm_exec.js"
