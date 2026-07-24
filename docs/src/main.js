import './style.css'
// Vendored, zero-dependency Web Components (lega4e/ui-kit). Importing the
// entry registers every <ga-*> element; the tokens give the page its theme.
import '../vendor/ui-kit/src/index.js'
import '../vendor/ui-kit/src/tokens/tokens.css'

const sdlEl = document.getElementById('sdl')
const sdl2El = document.getElementById('sdl2')
const gqlEl = document.getElementById('gql')
const varsEl = document.getElementById('vars')
const runEl = document.getElementById('run')
const statusEl = document.getElementById('status')

const deepEl = document.getElementById('deep')
const isdlEl = document.getElementById('isdl')
const iqueryEl = document.getElementById('iquery')

const migrationEl = document.getElementById('migration')
const sqlEl = document.getElementById('sql')
const paramsEl = document.getElementById('params')
const deltaEl = document.getElementById('delta')
const deepOutEl = document.getElementById('deepout')
const maxDepthEl = document.getElementById('maxdepth')
const isqlEl = document.getElementById('isql')
const igraphEl = document.getElementById('igraph')

// Load a classic script relative to the document (not the module bundle), so
// it resolves correctly under the PR-preview subpath.
function loadScript(src) {
  return new Promise((resolve, reject) => {
    const s = document.createElement('script')
    s.src = src
    s.onload = () => resolve()
    s.onerror = () => reject(new Error(`failed to load ${src}`))
    document.head.appendChild(s)
  })
}

// setCode writes text into a <ga-code> block. ga-code renders its default slot,
// so its textContent is both what is shown and what its copy button copies.
function setCode(el, text) {
  el.textContent = text
}

// graphSection pulls the CREATE PROPERTY GRAPH statement out of a generated
// migration, so the interface panel shows the shared LABEL clauses without the
// surrounding CREATE TABLEs.
function graphSection(migration) {
  const start = migration.indexOf('CREATE PROPERTY GRAPH')
  if (start < 0) return migration
  const end = migration.indexOf('\n-- +goose Down', start)
  return (end < 0 ? migration.slice(start) : migration.slice(start, end)).trim()
}

let ok = true

// render regenerates every output from the current inputs. Each generator runs
// independently, so a bad query still shows a valid migration, and vice versa.
function render() {
  ok = true

  const sdl = sdlEl.value
  const query = gqlEl.value
  const vars = varsEl.value
  const revised = sdl2El.value

  const mig = globalThis.gopgqlMigration(sdl)
  setCode(migrationEl, mig.error || mig.migration)
  if (mig.error) ok = false

  const cmp = globalThis.gopgqlCompile(sdl, query, vars)
  setCode(sqlEl, cmp.error || cmp.sql)
  setCode(paramsEl, cmp.error ? '—' : cmp.params)
  if (cmp.error) ok = false

  const dl = globalThis.gopgqlDelta(sdl, revised)
  setCode(deltaEl, dl.error || dl.delta)
  if (dl.error) ok = false

  // Depth limit. A rejection here is the designed outcome, not a page error:
  // gopgql refuses a selection past MaxDepth rather than truncating it, so a
  // typed depth error is a success for this panel and does not set `ok` false.
  const deep = globalThis.gopgqlCompile(sdl, deepEl.value, vars)
  if (deep.depthExceeded) {
    setCode(deepOutEl, `rejected at compile time — *compiler.DepthExceededError` +
      `\n\n${deep.error}\n\nMaxDepth = ${deep.maxDepth}. No SQL was emitted, so nothing reached a database.`)
  } else if (deep.error) {
    setCode(deepOutEl, deep.error)
    ok = false
  } else {
    setCode(deepOutEl, `compiled within MaxDepth ${deep.maxDepth}:\n\n${deep.sql}`)
  }

  // Interfaces: its own schema, so the main editor stays the worked example.
  const isdl = isdlEl.value
  const icmp = globalThis.gopgqlCompile(isdl, iqueryEl.value, '')
  setCode(isqlEl, icmp.error || icmp.sql)
  if (icmp.error) ok = false
  const imig = globalThis.gopgqlMigration(isdl)
  setCode(igraphEl, imig.error || graphSection(imig.migration))
  if (imig.error) ok = false

  if (ok) {
    statusEl.textContent = 'generated'
    statusEl.className = 'status ok'
  } else {
    statusEl.textContent = 'see errors below'
    statusEl.className = 'status error'
  }
}

async function boot() {
  try {
    // wasm_exec.js defines globalThis.Go. Fetched relative to the document so
    // it works at any base path.
    await loadScript('wasm_exec.js')
    const go = new globalThis.Go()
    const resp = await fetch('gopgql.wasm')
    if (!resp.ok) {
      throw new Error(`gopgql.wasm: HTTP ${resp.status}`)
    }
    const bytes = await resp.arrayBuffer()
    const { instance } = await WebAssembly.instantiate(bytes, go.importObject)
    // go.run resolves only when the Go program exits; our program blocks
    // forever (select{}), so we intentionally do not await it.
    go.run(instance)

    // The Go main sets the exported functions synchronously before it blocks.
    sdlEl.value = globalThis.gopgqlExampleSDL || sdlEl.value
    gqlEl.value = globalThis.gopgqlExampleQuery || gqlEl.value
    varsEl.value = globalThis.gopgqlExampleVars || varsEl.value
    sdl2El.value = globalThis.gopgqlRevisedExampleSDL || sdl2El.value
    deepEl.value = globalThis.gopgqlExampleDeepQuery || deepEl.value
    isdlEl.value = globalThis.gopgqlExampleInterfaceSDL || isdlEl.value
    iqueryEl.value = globalThis.gopgqlExampleInterfaceQuery || iqueryEl.value
    maxDepthEl.textContent = String(globalThis.gopgqlMaxDepth ?? 3)

    runEl.removeAttribute('disabled')
    statusEl.textContent = 'ready'
    statusEl.className = 'status ok'
    render()
  } catch (err) {
    statusEl.textContent = String(err)
    statusEl.className = 'status error'
  }
}

runEl.addEventListener('click', render)
// Live regeneration as the schema, query or variables are edited.
for (const el of [sdlEl, gqlEl, varsEl, sdl2El, deepEl, isdlEl, iqueryEl]) {
  el.addEventListener('input', () => {
    if (globalThis.gopgqlMigration) render()
  })
}

boot()
