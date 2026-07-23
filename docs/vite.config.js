import { defineConfig } from 'vite'

// base './' emits relative asset paths, which is required for the GitHub Pages
// PR-preview subpaths (e.g. /gopgql/pr-preview/pr-3/) — SPEC.md §8.3.
export default defineConfig({
  base: './',
})
