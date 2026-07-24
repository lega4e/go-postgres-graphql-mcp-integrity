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

const migrationEl = document.getElementById('migration')
const sqlEl = document.getElementById('sql')
const paramsEl = document.getElementById('params')
const deltaEl = document.getElementById('delta')

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
for (const el of [sdlEl, gqlEl, varsEl, sdl2El]) {
  el.addEventListener('input', () => {
    if (globalThis.gopgqlMigration) render()
  })
}

boot()
