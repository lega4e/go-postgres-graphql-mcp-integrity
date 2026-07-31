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
const REQUIRED_API_VERSION = 6

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
  setRunnable('traversal', ok, cmp)
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
  setRunnable('multipattern', ok, cmp)
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
  setRunnable('directives', ok, cmp)
}

// renderConstraints shows the M7 constraint directives in the generated DDL —
// defaults, named checks, and a natural key that arrives *alongside* the
// surrogate id — next to a query that selects a vertex by that key, which is
// what the KEY (...) clause in the graph makes possible.
function renderConstraints() {
  const sdl = valueOf('k-sdl')
  let ok = renderSchema('k-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, valueOf('k-query'), valueOf('k-vars'))
  setCode('k-sql', cmp.error || cmp.sql)
  setCode('k-params', cmp.error ? '—' : cmp.params)
  if (cmp.error) ok = false

  setStatus('k-status', ok, ok ? 'generated' : 'see errors')
  setRunnable('constraints', ok, cmp)
}

// renderRename diffs the revised schema against the one in the scenario above
// it, the same way the Migration tab does. The status line reports which of the
// two outcomes the delta took — a rename, or the drop-and-add a differ with no
// hint to go on has to fall back to — because that difference is the whole
// point of the scenario and is easy to miss in a wall of SQL.
function renderRename() {
  const dl = globalThis.gopgqlDelta(valueOf('k-sdl'), valueOf('k-sdl2'))
  setCode('k-delta', dl.error || dl.delta)
  if (dl.error) {
    setStatus('k-status2', false, 'see errors')
    return
  }
  if (!dl.changed) {
    setStatus('k-status2', true, 'no schema change')
    return
  }
  const renamed = dl.delta.includes('RENAME COLUMN') || dl.delta.includes('RENAME TO')
  const dropped = dl.delta.includes('DROP COLUMN')
  setStatus('k-status2', renamed && !dropped,
    renamed && !dropped ? 'renamed — the data moves with the column' : 'dropped and added — the data is gone')
}

// renderConformance generates the half of the check a browser can do: the graph
// mapping the SDL describes, which is the entire surface conform compares. The
// report beside it is a fixture — the conform package needs a live database and
// so is not part of the WASM surface (SPEC.md §4.1) — and both the panel and
// the label say so rather than letting it pass as a live result.
function renderConformance() {
  setCode('f-sdl', globalThis.gopgqlExampleConstraintsSDL ?? '')
  const out = globalThis.gopgqlGraph(globalThis.gopgqlExampleConstraintsSDL ?? '')
  setCode('f-graph', out.error || out.graph)
  setCode('f-report', globalThis.gopgqlConformanceReport ?? '')
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
    // There is no SQL to run, which is the point of the tab: nothing reached a
    // database because nothing was emitted. Offering Run here would suggest
    // there was something to send.
    setRunnable('depth', false, cmp)
    return
  }
  setCode('d-sql', cmp.error || cmp.sql)
  const ok = schemaOk && !cmp.error
  setStatus('d-status', ok, ok ? `compiled within MaxDepth ${cmp.maxDepth}` : 'see errors')
  setRunnable('depth', ok, cmp)
}

function renderInterfaces() {
  const sdl = valueOf('i-sdl')
  let ok = renderSchema('i-schema', sdl)

  const cmp = globalThis.gopgqlCompile(sdl, valueOf('i-query'), '')
  setCode('i-sql', cmp.error || cmp.sql)
  if (cmp.error) ok = false

  setStatus('i-status', ok, ok ? 'generated' : 'see errors')
  setRunnable('interfaces', ok, cmp)
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

// --- execution ------------------------------------------------------------
//
// Everything above generates. Everything below runs what was generated against
// a real PostgreSQL, compiled to WebAssembly, in the reader's own browser.
//
// The runtime is ~15 MB and is not fetched until someone presses Run. The
// module specifier lives in exactly one place — a dynamic import inside
// pglite-worker.js — and the worker itself is not constructed until the first
// Run, so nothing on the boot path can reach it.

// executions lists the scenarios that compile a query and can therefore be
// executed. Rename, Delta, Migration and Conformance are absent because they
// produce no query: a Run control on them would have nothing to send.
//
// `schema` and `sql` are the *panes*, not the values. Run reads the SQL it
// executes out of the pane the reader is looking at, so "the SQL shown is the
// SQL executed" holds by construction rather than by convention.
const executions = {
  traversal: {
    schema: 't-schema', sql: 't-sql', panel: 't-result', status: 't-exec-status',
    seed: () => globalThis.gopgqlExampleSeed,
  },
  multipattern: {
    schema: 'p-schema', sql: 'p-sql', panel: 'p-result', status: 'p-exec-status',
    seed: () => globalThis.gopgqlExampleSeed,
  },
  directives: {
    schema: 'c-schema', sql: 'c-sql', panel: 'c-result', status: 'c-exec-status',
    seed: () => globalThis.gopgqlExampleDirectivesSeed,
  },
  constraints: {
    schema: 'k-schema', sql: 'k-sql', panel: 'k-result', status: 'k-exec-status',
    seed: () => globalThis.gopgqlExampleConstraintsSeed,
  },
  depth: {
    schema: 'd-schema', sql: 'd-sql', panel: 'd-result', status: 'd-exec-status',
    // The Depth tab shares the Traversal schema, so it shares its seed. It is
    // runnable only when the ceiling was raised enough for a query to exist.
    seed: () => globalThis.gopgqlExampleSeed,
  },
  interfaces: {
    schema: 'i-schema', sql: 'i-sql', panel: 'i-result', status: 'i-exec-status',
    seed: () => globalThis.gopgqlExampleInterfaceSeed,
  },
}

// runState is per scenario: whether the last generation produced something
// runnable, the bind values that go with it, and whether a run is in flight.
const runState = new Map()

// One worker for the whole page, built on the first Run and reused after it, so
// a second Run does not re-download anything.
let worker = null
// runtimeUnavailable holds the reason the runtime could not be loaded, once it
// has failed. Everything else on the page keeps working.
let runtimeUnavailable = ''
let nextRunId = 0
const pending = new Map()

/** The Run control for a scenario, if the markup declares one. */
function execButton(name) {
  return document.querySelector(`.exec[data-scenario="${name}"]`)
}

/**
 * decodeArgs turns the compile result's `args` — a JSON array string — into the
 * ordered values to bind. A module that predates this surface would omit the
 * field entirely; the API version check refuses that pairing before any of this
 * runs, so an empty array here means "this query binds nothing", never "the
 * values were lost".
 */
function decodeArgs(raw) {
  if (!raw) return []
  try {
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

/**
 * setRunnable records what the last generation left behind. A scenario whose
 * compile failed, or whose query was refused for exceeding the depth ceiling,
 * is not runnable and its control says so.
 */
function setRunnable(name, ok, compiled) {
  if (!executions[name]) return
  const prior = runState.get(name)
  const state = {
    ok: Boolean(ok) && Boolean(compiled?.sql),
    args: ok ? decodeArgs(compiled?.args) : [],
    running: prior?.running ?? false,
  }
  runState.set(name, state)

  const btn = execButton(name)
  if (!btn) return
  const blocked = !state.ok || state.running || runtimeUnavailable
  if (blocked) btn.setAttribute('disabled', '')
  else btn.removeAttribute('disabled')
}

// --- the worker ------------------------------------------------------------

function failAllPending(reason) {
  runtimeUnavailable = reason
  for (const { reject } of pending.values()) reject(new Error(reason))
  pending.clear()
  for (const name of Object.keys(executions)) {
    const btn = execButton(name)
    if (btn) btn.setAttribute('disabled', '')
  }
}

function ensureWorker() {
  if (worker) return worker
  // new URL(..., import.meta.url) is what lets Vite emit the worker as its own
  // chunk with a path relative to the bundle — which is what makes it resolve
  // under a pr-preview/pr-<N>/ subpath as well as at the site root.
  worker = new Worker(new URL('./pglite-worker.js', import.meta.url), { type: 'module' })
  worker.addEventListener('message', (event) => {
    const msg = event.data
    const entry = pending.get(msg?.id)
    if (!entry) return
    pending.delete(msg.id)
    if (msg.type === 'error') entry.reject(new Error(msg.message))
    else entry.resolve(msg)
  })
  // A worker that fails to start — or a runtime that fails to instantiate
  // inside it — surfaces here rather than as a message.
  worker.addEventListener('error', (event) => {
    failAllPending(event.message || 'the PostgreSQL runtime failed to load')
  })
  return worker
}

function postRun(request) {
  const w = ensureWorker()
  const id = ++nextRunId
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject })
    w.postMessage({ type: 'run', id, ...request })
  })
}

// --- rendering a result ----------------------------------------------------

/** Empty a panel and hand it back for rebuilding. */
function resetPanel(id) {
  const panel = el(id)
  panel.textContent = ''
  return panel
}

/** A short line of prose in the result panel. */
function appendNote(panel, text, kind) {
  const p = document.createElement('p')
  p.className = kind ? `result-note ${kind}` : 'result-note'
  p.textContent = text
  panel.appendChild(p)
}

/**
 * PostgreSQL's own message, verbatim, under the name of the step that raised
 * it. Watching PostgreSQL accept or reject generated SQL is the feature, so
 * nothing here paraphrases what it said.
 */
function appendDatabaseError(panel, step, message) {
  appendNote(panel, `${step} failed:`, 'error')
  const pre = document.createElement('pre')
  pre.className = 'result-error'
  pre.textContent = message
  panel.appendChild(pre)
}

/** The rows, as a table with the result's own column names. */
function appendTable(panel, columns, rows) {
  const scroll = document.createElement('div')
  scroll.className = 'result-scroll'
  const table = document.createElement('table')
  table.className = 'result-table'

  const head = document.createElement('thead')
  const headRow = document.createElement('tr')
  for (const name of columns) {
    const th = document.createElement('th')
    th.textContent = name
    headRow.appendChild(th)
  }
  head.appendChild(headRow)
  table.appendChild(head)

  const body = document.createElement('tbody')
  for (const row of rows) {
    const tr = document.createElement('tr')
    for (const value of row) {
      const td = document.createElement('td')
      // A SQL NULL and the empty string are different results and have to look
      // different.
      if (value === null) {
        td.textContent = 'NULL'
        td.className = 'null'
      } else {
        td.textContent = String(value)
      }
      tr.appendChild(td)
    }
    body.appendChild(tr)
  }
  table.appendChild(body)
  scroll.appendChild(table)
  panel.appendChild(scroll)
}

/**
 * renderRun writes one run's outcome. Every step is an outcome, not an
 * exception: a rejected statement, an inapplicable seed and an empty result are
 * all things PostgreSQL did, and the panel says which.
 */
function renderRun(spec, outcome) {
  const panel = resetPanel(spec.panel)
  const { steps, version } = outcome

  if (!steps.schema?.ok) {
    appendDatabaseError(panel, 'Applying the generated schema', steps.schema?.error ?? 'unknown error')
    setStatus(spec.status, false, 'schema rejected')
    return
  }

  // The seed is a fixture bound to an *example* schema. An edited schema it no
  // longer fits is not a page error — the query still runs, and returns what it
  // honestly finds.
  if (steps.seed && !steps.seed.ok) {
    appendNote(panel,
      'The seed data did not apply to this schema — it is a fixture for the ' +
      'example schema, and you have edited it. The query below still ran.',
      'warn')
    const pre = document.createElement('pre')
    pre.className = 'result-error'
    pre.textContent = steps.seed.error
    panel.appendChild(pre)
  }

  if (!steps.query?.ok) {
    appendDatabaseError(panel, 'Executing the query', steps.query?.error ?? 'unknown error')
    setStatus(spec.status, false, 'query rejected')
    return
  }

  const { columns, rows } = steps.query
  if (rows.length === 0) {
    appendNote(panel, 'The query succeeded and returned no rows.')
  } else {
    appendTable(panel, columns, rows)
  }
  // Provenance: what PostgreSQL says about itself, and which pinned build of
  // the fork that PostgreSQL came from. __PGLITE_BUILD__ is derived at build
  // time from the pin in package.json, so it cannot drift from what was
  // actually loaded.
  if (version) appendNote(panel, version, 'provenance')
  appendNote(panel, __PGLITE_BUILD__, 'provenance')

  setStatus(spec.status, true,
    rows.length === 1 ? '1 row' : `${rows.length} rows`)
}

/**
 * execute runs one scenario: the generated DDL, then the scenario's seed, then
 * the SQL in its pane with the bind values from its last compile.
 *
 * gopgqlSchema's DDL is what runs — not gopgqlMigration's document, which is
 * goose-annotated across two files and would need a migration tool's annotation
 * parser to apply.
 */
async function execute(name) {
  const spec = executions[name]
  const state = runState.get(name)
  if (!spec || !state?.ok || state.running) return

  const btn = execButton(name)
  state.running = true
  if (btn) btn.setAttribute('disabled', '')
  setStatus(spec.status, true, 'running — this may take a few seconds…')
  const panel = resetPanel(spec.panel)
  appendNote(panel, 'Starting PostgreSQL in your browser…')

  const request = {
    ddl: valueOf(spec.schema),
    seed: spec.seed() ?? '',
    sql: valueOf(spec.sql),
    args: state.args,
  }
  // A debugging affordance, and what the browser suite asserts against to prove
  // the executed SQL is the SQL on the page.
  globalThis.gopgqlLastRun = { scenario: name, ...request }

  try {
    renderRun(spec, await postRun(request))
  } catch (err) {
    const reason = String(err?.message ?? err)
    const failed = resetPanel(spec.panel)
    appendNote(failed,
      `Execution is unavailable: ${reason}. Everything above is still ` +
      'generated by the WebAssembly module and is unaffected.', 'error')
    setStatus(spec.status, false, 'runtime unavailable')
  } finally {
    state.running = false
    if (btn && !runtimeUnavailable && state.ok) btn.removeAttribute('disabled')
  }
}

// scenarios maps each Generate button (and each live-edited input) to the
// renderer it drives. An input listed under two scenarios re-runs both — the
// migration SDL feeds the delta as well as the initial migration.
const scenarios = {
  traversal: { render: renderTraversal, inputs: ['t-sdl', 't-query', 't-vars'] },
  multipattern: { render: renderMultiPattern, inputs: ['p-sdl', 'p-query', 'p-vars'] },
  directives: { render: renderDirectives, inputs: ['c-sdl', 'c-query', 'c-vars'] },
  constraints: { render: renderConstraints, inputs: ['k-sdl', 'k-query', 'k-vars'] },
  rename: { render: renderRename, inputs: ['k-sdl', 'k-sdl2'] },
  // The Conformance panel has no editable input: its report is a recording
  // taken against one schema, and letting that schema be edited would make the
  // report describe something that is no longer on the page.
  conformance: { render: renderConformance, inputs: [] },
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

    seed('k-sdl', globalThis.gopgqlExampleConstraintsSDL)
    seed('k-query', globalThis.gopgqlExampleConstraintsQuery)
    seed('k-vars', globalThis.gopgqlExampleConstraintsVars)
    seed('k-sdl2', globalThis.gopgqlRevisedConstraintsSDL)

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

for (const name of Object.keys(executions)) {
  const btn = execButton(name)
  if (btn) btn.addEventListener('click', () => execute(name))
}

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
