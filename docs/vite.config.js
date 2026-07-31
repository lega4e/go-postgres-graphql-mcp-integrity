import { readFileSync } from 'node:fs'
import { defineConfig } from 'vite'

// The pinned runtime, described for the page from the pin itself rather than
// from a second copy of the same facts. A reader who has just run a query is
// entitled to know which build produced the rows, and `SELECT version()` names
// PostgreSQL and emscripten but not the release this site pinned.
//
// Deriving it here means bumping the pin updates the page; nothing to forget.
const pglitePin = JSON.parse(
  readFileSync(new URL('./package.json', import.meta.url), 'utf8'))
  .devDependencies['@electric-sql/pglite']
// .../releases/download/<tag>/<tarball> — the tag is the release, and the
// tarball's name carries the package version.
const pinnedTag = pglitePin.split('/download/')[1]?.split('/')[0] ?? 'unpinned'
const pinnedPackage = pglitePin
  .split('/').pop()
  ?.replace(/^electric-sql-pglite-/, '@electric-sql/pglite@')
  .replace(/\.tgz$/, '') ?? 'unknown'

// base './' emits relative asset paths, which is required for the GitHub Pages
// PR-preview subpaths (e.g. /gopgql/pr-preview/pr-3/) — SPEC.md §8.3.
export default defineConfig({
  base: './',
  define: {
    __PGLITE_BUILD__: JSON.stringify(
      `fork build: postgres-pglite ${pinnedTag} — ${pinnedPackage}`),
  },
  build: {
    // The manifest records, per chunk, which imports are static and which are
    // dynamic. scripts/check-lazy-runtime.mjs walks the static graph with it to
    // prove nothing on the boot path can reach pglite.wasm; without it the
    // check would have to re-parse the bundle's import statements by hand.
    manifest: true,
  },
  optimizeDeps: {
    // PGlite must not go through Vite's dependency pre-bundling. Pre-bundling
    // rewrites the module but not the URLs it computes for pglite.wasm and
    // pglite.data, so in dev the runtime asks for assets that are not where it
    // says they are. The fork's own bundler-support notes require this
    // exclusion; it is not a preference.
    exclude: ['@electric-sql/pglite'],
  },
  worker: {
    // The PGlite worker is constructed with { type: 'module' } and loads the
    // runtime through a dynamic import, so the emitted worker has to be an ES
    // module. Vite's default worker format is 'iife', which cannot code-split
    // and would inline the runtime into the worker's first byte.
    format: 'es',
  },
})
