import './style.css'
import { createInput, createOutput, getDoc, setDoc } from './editor.js'
// Vendored, zero-dependency Web Components (lega4e/ui-kit). Importing the
// entry registers every <ga-*> element; the tokens give the page its theme.
import '../vendor/ui-kit/src/index.js'
import '../vendor/ui-kit/src/tokens/tokens.css'

// REQUIRED_API_VERSION must match apiVersion in cmd/wasm/main.go. gopgql.wasm is
// a build artefact served from an unhashed URL, so a new page can end up running
// an old module — which would silently ignore arguments this page passes rather
// than failing. Checking the version turns that into a message that says what to
// do about it.
const REQUIRED_API_VERSION = 4

const el = (id) => document.getElementById(id)

// Every upgraded pane, by the id its <textarea>/<ga-code> had. The rest of the
// page keeps addressing panes by id and never learns they became editors.
const editors = new Map()

/** The text in a pane: the editor's document, or a plain input's value. */
function valueOf(id) {
  const view = editors.get(id)
  return view ? getDoc(view) : el(id).value
}

/** Replace a pane's text. */
function setValue(id, text) {
  const view = editors.get(id)
  if (view) setDoc(view, text)
  else el(id).value = text
}

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

// setCode writes a generated pane: the read-only editor it was upgraded to, or
// the original <ga-code> block when the upgrade did not run.
function setCode(id, text) {
  const view = editors.get(id)
  if (view) setDoc(view, text ?? '')
  else el(id).textContent = text
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
  const sdl = valueOf('t-sdl')
  let ok = renderSchema('t-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, valueOf('t-query'), valueOf('t-vars'))
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
  const sdl = valueOf('p-sdl')
  let ok = renderSchema('p-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, valueOf('p-query'), valueOf('p-vars'))
  setCode('p-sql', cmp.error || cmp.sql)
  setCode('p-params', cmp.error ? '—' : cmp.params)
  if (cmp.error) ok = false

  const calls = cmp.error ? 0 : (cmp.sql.match(/GRAPH_TABLE/g) ?? []).length
  setStatus('p-status', ok,
    ok ? `${calls} GRAPH_TABLE call${calls === 1 ? '' : 's'}` : 'see errors')
}

// renderDirectives shows what the M6 mapping directives do to the generated
// DDL, next to the query compiled against the same schema: @column(name:)
// renames the column the graph exposes, so the compiled SQL projects the column
// while the GraphQL field keeps its own name.
function renderDirectives() {
  const sdl = valueOf('c-sdl')
  let ok = renderSchema('c-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, valueOf('c-query'), valueOf('c-vars'))
  setCode('c-sql', cmp.error || cmp.sql)
  if (cmp.error) ok = false

  setStatus('c-status', ok, ok ? 'generated' : 'see errors')
}

// depthLimit reads the Max depth input, falling back to the compiler's own
// default when it is blank or not a number. Negatives are clamped here the way
// the compiler clamps them, so the ceiling reported back is the one applied.
function depthLimit() {
  const n = Number.parseInt(valueOf('d-max'), 10)
  return Number.isFinite(n) ? Math.max(0, n) : (globalThis.gopgqlMaxDepth ?? 3)
}

// renderDepth treats a depth rejection as success: refusing to truncate a
// pattern past MaxDepth is the designed outcome, not a page error.
function renderDepth() {
  const sdl = valueOf('d-sdl')
  const schemaOk = renderSchema('d-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, valueOf('d-query'), valueOf('d-vars'), depthLimit())
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
  const sdl = valueOf('i-sdl')
  let ok = renderSchema('i-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, valueOf('i-query'), '')
  setCode('i-sql', cmp.error || cmp.sql)
  if (cmp.error) ok = false

  setStatus('i-status', ok, ok ? 'generated' : 'see errors')
}

function renderMigration() {
  const mig = globalThis.gopgqlMigration(valueOf('m-sdl'))
  setCode('m-init', mig.error || mig.migration)
  setStatus('m-status', !mig.error, mig.error ? 'see errors' : 'generated')
}

// renderDelta diffs the revised schema against the one in the scenario above it,
// which is why editing either textarea re-runs it.
function renderDelta() {
  const dl = globalThis.gopgqlDelta(valueOf('m-sdl'), valueOf('m-sdl2'))
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
  directives: { render: renderDirectives, inputs: ['c-sdl', 'c-query', 'c-vars'] },
  depth: { render: renderDepth, inputs: ['d-sdl', 'd-query', 'd-vars', 'd-max'] },
  interfaces: { render: renderInterfaces, inputs: ['i-sdl', 'i-query'] },
  migration: { render: renderMigration, inputs: ['m-sdl'] },
  delta: { render: renderDelta, inputs: ['m-sdl', 'm-sdl2'] },
}

// onPaneEdited re-renders every scenario that reads the edited pane. An input
// feeding two scenarios (the migration SDL feeds the delta as well) re-runs
// both, which is what the textarea listeners used to do.
function onPaneEdited(id) {
  if (!globalThis.gopgqlSchema) return
  for (const s of Object.values(scenarios)) {
    if (s.inputs.includes(id)) s.render()
  }
}

/**
 * Turn the static markup into editors: every `data-lang` textarea becomes an
 * editable CodeMirror pane, and every `data-lang` <ga-code> becomes a read-only
 * one with its own copy button. Panes keep their ids, so the rest of the page
 * is unchanged — and if this never runs, the original markup still works.
 */
function upgradeEditors() {
  for (const ta of document.querySelectorAll('textarea.code-input[data-lang]')) {
    const id = ta.id
    const wrap = document.createElement('div')
    wrap.className = 'code-editor'
    wrap.id = id
    ta.removeAttribute('id')
    ta.replaceWith(wrap)
    const view = createInput({
      parent: wrap,
      doc: ta.value,
      lang: ta.dataset.lang,
      minHeight: `${Math.max(Number(ta.rows) || 3, 2) * 1.5}em`,
      onChange: () => onPaneEdited(id),
    })
    editors.set(id, view)
    // A <div> is not labelable, so carry the label across by hand.
    const label = document.querySelector(`label[for="${id}"]`)
    if (label) {
      wrap.setAttribute('aria-label', label.textContent.trim())
      label.addEventListener('click', () => view.focus())
    }
  }

  for (const code of document.querySelectorAll('ga-code[data-lang]')) {
    const id = code.id
    const wrap = document.createElement('div')
    wrap.className = 'code-view'
    wrap.id = id
    const copy = document.createElement('button')
    copy.type = 'button'
    copy.className = 'code-copy'
    copy.textContent = 'Copy'
    copy.addEventListener('click', async () => {
      await navigator.clipboard.writeText(getDoc(editors.get(id)))
      copy.textContent = 'Copied'
      setTimeout(() => { copy.textContent = 'Copy' }, 1200)
    })
    code.replaceWith(wrap)
    const view = createOutput({ parent: wrap, doc: code.textContent.trim(), lang: code.dataset.lang })
    editors.set(id, view)
    wrap.appendChild(copy)
  }
}

function renderAll() {
  for (const s of Object.values(scenarios)) s.render()
}

// seed fills an input from a value the WASM module exported, leaving whatever is
// there if the export is missing.
function seed(id, value) {
  if (value) setValue(id, value)
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

    seed('c-sdl', globalThis.gopgqlExampleDirectivesSDL)
    seed('c-query', globalThis.gopgqlExampleDirectivesQuery)
    seed('c-vars', globalThis.gopgqlExampleDirectivesVars)

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

upgradeEditors()

for (const [name, scenario] of Object.entries(scenarios)) {
  for (const btn of document.querySelectorAll(`.run[data-scenario="${name}"]`)) {
    btn.addEventListener('click', scenario.render)
  }
  // Live regeneration as the schema or query is edited. Editor panes report
  // their own changes through onPaneEdited; anything left as a plain input
  // (the depth spinner) still needs a listener.
  for (const id of scenario.inputs) {
    if (editors.has(id)) continue
    el(id).addEventListener('input', () => {
      if (globalThis.gopgqlSchema) scenario.render()
    })
  }
}

boot()
