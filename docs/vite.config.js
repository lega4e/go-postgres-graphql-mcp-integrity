import { defineConfig } from 'vite'

// base './' emits relative asset paths, which is required for the GitHub Pages
// PR-preview subpaths (e.g. /gopgql/pr-preview/pr-3/) — SPEC.md §8.3.
export default defineConfig({
  base: './',
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
