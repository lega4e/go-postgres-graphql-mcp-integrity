import './style.css'
// Vendored, zero-dependency Web Components (lega4e/ui-kit). Importing the
// entry registers every <ga-*> element; the tokens give the page its theme.
import '../vendor/ui-kit/src/index.js'
import '../vendor/ui-kit/src/tokens/tokens.css'

// REQUIRED_API_VERSION must match apiVersion in cmd/wasm/main.go. gopgql.wasm is
// a build artefact served from an unhashed URL, so a new page can end up running
// an old module — which would silently ignore arguments this page passes rather
// than failing. Checking the version turns that into a message that says what to
// do about it.
const REQUIRED_API_VERSION = 2

const el = (id) => document.getElementById(id)

const bootEl = el('boot')
const maxDepthEl = el('maxdepth')

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
function setCode(id, text) {
  el(id).textContent = text
}

function setStatus(id, ok, message) {
  const node = el(id)
  node.textContent = message
  node.className = ok ? 'status ok' : 'status error'
}

// renderSchema writes the DDL an SDL generates, and reports whether it is valid.
// Every scenario shows it: the compiled query only means something next to the
// schema it runs against.
function renderSchema(outId, sdl) {
  const out = globalThis.gopgqlSchema(sdl)
  setCode(outId, out.error || out.schema)
  return !out.error
}

// Each scenario below is one tab: it reads only its own inputs and writes only
// its own outputs, so a broken query in one tab leaves the others alone.

function renderTraversal() {
  const sdl = el('t-sdl').value
  let ok = renderSchema('t-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, el('t-query').value, el('t-vars').value)
  setCode('t-sql', cmp.error || cmp.sql)
  setCode('t-params', cmp.error ? '—' : cmp.params)
  if (cmp.error) ok = false

  setStatus('t-status', ok, ok ? 'generated' : 'see errors')
}

// renderMultiPattern shows the M5 workaround: the same call as Traversal, but on
// a query that branches, so the SQL pane holds the split GRAPH_TABLE calls and
// their joins. The count is reported in the status line, since "how many
// statements did this become" is the point of the tab.
function renderMultiPattern() {
  const sdl = el('p-sdl').value
  let ok = renderSchema('p-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, el('p-query').value, el('p-vars').value)
  setCode('p-sql', cmp.error || cmp.sql)
  setCode('p-params', cmp.error ? '—' : cmp.params)
  if (cmp.error) ok = false

  const calls = cmp.error ? 0 : (cmp.sql.match(/GRAPH_TABLE/g) ?? []).length
  setStatus('p-status', ok,
    ok ? `${calls} GRAPH_TABLE call${calls === 1 ? '' : 's'}` : 'see errors')
}

// depthLimit reads the Max depth input, falling back to the compiler's own
// default when it is blank or not a number. Negatives are clamped here the way
// the compiler clamps them, so the ceiling reported back is the one applied.
function depthLimit() {
  const n = Number.parseInt(el('d-max').value, 10)
  return Number.isFinite(n) ? Math.max(0, n) : (globalThis.gopgqlMaxDepth ?? 3)
}

// renderDepth treats a depth rejection as success: refusing to truncate a
// pattern past MaxDepth is the designed outcome, not a page error.
function renderDepth() {
  const sdl = el('d-sdl').value
  const schemaOk = renderSchema('d-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, el('d-query').value, el('d-vars').value, depthLimit())
  if (cmp.depthExceeded) {
    setCode('d-sql',
      'rejected at compile time — *compiler.DepthExceededError\n\n' +
      `${cmp.error}\n\n` +
      `MaxDepth = ${cmp.maxDepth}. No SQL was emitted, so nothing reached a database.`)
    setStatus('d-status', schemaOk, schemaOk ? 'rejected, as designed' : 'see errors')
    return
  }
  setCode('d-sql', cmp.error || cmp.sql)
  const ok = schemaOk && !cmp.error
  setStatus('d-status', ok, ok ? `compiled within MaxDepth ${cmp.maxDepth}` : 'see errors')
}

function renderInterfaces() {
  const sdl = el('i-sdl').value
  let ok = renderSchema('i-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, el('i-query').value, '')
  setCode('i-sql', cmp.error || cmp.sql)
  if (cmp.error) ok = false

  setStatus('i-status', ok, ok ? 'generated' : 'see errors')
}

function renderMigration() {
  const mig = globalThis.gopgqlMigration(el('m-sdl').value)
  setCode('m-init', mig.error || mig.migration)
  setStatus('m-status', !mig.error, mig.error ? 'see errors' : 'generated')
}

// renderDelta diffs the revised schema against the one in the scenario above it,
// which is why editing either textarea re-runs it.
function renderDelta() {
  const dl = globalThis.gopgqlDelta(el('m-sdl').value, el('m-sdl2').value)
  setCode('m-delta', dl.error || dl.delta)
  if (dl.error) {
    setStatus('m-status2', false, 'see errors')
    return
  }
  setStatus('m-status2', true, dl.changed ? 'generated' : 'no schema change')
}

// scenarios maps each Generate button (and each live-edited input) to the
// renderer it drives. An input listed under two scenarios re-runs both — the
// migration SDL feeds the delta as well as the initial migration.
const scenarios = {
  traversal: { render: renderTraversal, inputs: ['t-sdl', 't-query', 't-vars'] },
  multipattern: { render: renderMultiPattern, inputs: ['p-sdl', 'p-query', 'p-vars'] },
  depth: { render: renderDepth, inputs: ['d-sdl', 'd-query', 'd-vars', 'd-max'] },
  interfaces: { render: renderInterfaces, inputs: ['i-sdl', 'i-query'] },
  migration: { render: renderMigration, inputs: ['m-sdl'] },
  delta: { render: renderDelta, inputs: ['m-sdl', 'm-sdl2'] },
}

function renderAll() {
  for (const s of Object.values(scenarios)) s.render()
}

// seed fills an input from a value the WASM module exported, leaving whatever is
// there if the export is missing.
function seed(id, value) {
  if (value) el(id).value = value
}

async function boot() {
  try {
    // wasm_exec.js defines globalThis.Go. Fetched relative to the document so
    // it works at any base path.
    await loadScript('wasm_exec.js')
    const go = new globalThis.Go()
    // no-cache revalidates rather than trusting a cached copy: the module's URL
    // never changes between builds, unlike the content-hashed JS bundle, so a
    // returning visitor would otherwise pair new page code with an old module.
    const resp = await fetch('gopgql.wasm', { cache: 'no-cache' })
    if (!resp.ok) {
      throw new Error(`gopgql.wasm: HTTP ${resp.status}`)
    }
    const bytes = await resp.arrayBuffer()
    const { instance } = await WebAssembly.instantiate(bytes, go.importObject)
    // go.run resolves only when the Go program exits; our program blocks
    // forever (select{}), so we intentionally do not await it.
    go.run(instance)

    const api = globalThis.gopgqlApiVersion ?? 0
    if (api !== REQUIRED_API_VERSION) {
      throw new Error(
        `gopgql.wasm is out of date: it exports API v${api || '(unknown)'}, ` +
        `this page needs v${REQUIRED_API_VERSION}. Rebuild it with ` +
        '`bash scripts/build-wasm.sh`, then reload.')
    }

    // The Go main sets the exported functions and examples synchronously
    // before it blocks.
    seed('t-sdl', globalThis.gopgqlExampleSDL)
    seed('t-query', globalThis.gopgqlExampleQuery)
    seed('t-vars', globalThis.gopgqlExampleVars)

    seed('p-sdl', globalThis.gopgqlExampleSDL)
    seed('p-query', globalThis.gopgqlExampleMultiQuery)
    seed('p-vars', globalThis.gopgqlExampleVars)

    seed('d-sdl', globalThis.gopgqlExampleSDL)
    seed('d-query', globalThis.gopgqlExampleDeepQuery)
    seed('d-vars', globalThis.gopgqlExampleVars)
    seed('d-max', String(globalThis.gopgqlMaxDepth ?? 3))

    seed('i-sdl', globalThis.gopgqlExampleInterfaceSDL)
    seed('i-query', globalThis.gopgqlExampleInterfaceQuery)

    seed('m-sdl', globalThis.gopgqlExampleSDL)
    seed('m-sdl2', globalThis.gopgqlRevisedExampleSDL)

    maxDepthEl.textContent = String(globalThis.gopgqlMaxDepth ?? 3)

    for (const btn of document.querySelectorAll('.run')) {
      btn.removeAttribute('disabled')
    }
    bootEl.textContent = 'WebAssembly ready — every output below is generated from your input'
    bootEl.className = 'status ok'
    renderAll()
  } catch (err) {
    bootEl.textContent = String(err)
    bootEl.className = 'status error'
  }
}

for (const [name, scenario] of Object.entries(scenarios)) {
  for (const btn of document.querySelectorAll(`.run[data-scenario="${name}"]`)) {
    btn.addEventListener('click', scenario.render)
  }
  // Live regeneration as the schema or query is edited.
  for (const id of scenario.inputs) {
    el(id).addEventListener('input', () => {
      if (globalThis.gopgqlSchema) scenario.render()
    })
  }
}

boot()
