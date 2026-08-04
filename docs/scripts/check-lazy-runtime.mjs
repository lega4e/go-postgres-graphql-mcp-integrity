// Fail the build if the PostgreSQL runtime stopped being lazy.
//
// The whole cost argument for shipping a 15 MB PostgreSQL with a static site is
// that a reader who never presses Run never pays for it. That property is one
// bundler upgrade, one stray import or one `optimizeDeps` change away from
// quietly disappearing, and nothing about the page would look different when it
// did. So it is asserted here rather than intended.
//
// The check reads Vite's build manifest, walks the *static* import graph from
// every entry, and requires that nothing reachable that way mentions
// pglite.wasm or pglite.data. Dynamic imports are deliberately not followed:
// being reachable only through one is exactly what "lazy" means.

import { readFileSync, existsSync, readdirSync, statSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const docsDir = join(dirname(fileURLToPath(import.meta.url)), '..')
const distDir = join(docsDir, 'dist')
const manifestPath = join(distDir, '.vite', 'manifest.json')

/** The binary assets that must never be reachable eagerly. */
const RUNTIME_ASSETS = ['pglite.wasm', 'pglite.data']

function fail(message) {
  console.error(`check-lazy-runtime: ${message}`)
  process.exit(1)
}

if (!existsSync(manifestPath)) {
  fail(`no build manifest at ${manifestPath} — is build.manifest still enabled in vite.config.js?`)
}

const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'))

/** Every file under dist, so the check can prove it is looking at something. */
function walk(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else out.push(full)
  }
  return out
}

const allFiles = walk(distDir)

/** Vite content-hashes asset filenames, so pglite.wasm ships as pglite-HASH.wasm. */
function isRuntimeAsset(file, asset) {
  const [stem, ext] = asset.split('.')
  const base = file.slice(file.lastIndexOf('/') + 1)
  return base.startsWith(stem) && base.endsWith(`.${ext}`)
}

// A vacuous pass is worse than a failure: if the runtime is not in the build at
// all, "nothing references it eagerly" is true and meaningless.
const runtimeFiles = allFiles.filter((f) =>
  RUNTIME_ASSETS.some((asset) => isRuntimeAsset(f, asset)))

if (runtimeFiles.length === 0) {
  fail('neither pglite.wasm nor pglite.data is in the build — the runtime is not being shipped at all')
}

// --- the eager graph -------------------------------------------------------

// Static imports only. `dynamicImports` is skipped on purpose.
const eager = new Set()
function collect(key) {
  if (eager.has(key)) return
  const entry = manifest[key]
  if (!entry) return
  eager.add(key)
  for (const imported of entry.imports ?? []) collect(imported)
}

const entries = Object.keys(manifest).filter((k) => manifest[k].isEntry)
if (entries.length === 0) fail('the manifest declares no entry chunk')
for (const key of entries) collect(key)

// --- the assertion ---------------------------------------------------------

const offenders = []
for (const key of eager) {
  const file = join(distDir, manifest[key].file)
  if (!existsSync(file)) continue
  const source = readFileSync(file, 'utf8')
  for (const asset of RUNTIME_ASSETS) {
    const stem = asset.split('.')[0]
    const ext = asset.split('.')[1]
    // Vite content-hashes the filename, so match the shape rather than the
    // literal name: pglite-DEADBEEF.wasm has to be caught too.
    if (new RegExp(`${stem}[-.][A-Za-z0-9_-]*\\.${ext}`).test(source) ||
        source.includes(asset)) {
      offenders.push(`${manifest[key].file} references ${asset}`)
    }
  }
}

// index.html itself must not preload them either — a modulepreload or a
// <link rel=preload> would defeat the whole thing without appearing in any
// chunk's source.
const indexHtml = join(distDir, 'index.html')
if (existsSync(indexHtml)) {
  const html = readFileSync(indexHtml, 'utf8')
  for (const asset of RUNTIME_ASSETS) {
    const stem = asset.split('.')[0]
    if (new RegExp(`${stem}[-.][A-Za-z0-9_-]*\\.${asset.split('.')[1]}`).test(html)) {
      offenders.push(`index.html references ${asset}`)
    }
  }
}

if (offenders.length > 0) {
  fail(
    'the PostgreSQL runtime is reachable without pressing Run:\n  ' +
    offenders.join('\n  ') +
    '\nEvery visitor would now download it. The runtime must be reached only ' +
    'through the dynamic import in src/pglite-worker.js.')
}

console.log(
  `check-lazy-runtime: ok — ${eager.size} eagerly-loaded chunk(s), none referencing ` +
  `${RUNTIME_ASSETS.join(' or ')}; ${runtimeFiles.length} runtime asset(s) present in the build.`)
