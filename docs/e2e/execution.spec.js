// The playground's execution feature, proven the only way it can be: by
// running it in a real browser.
//
// Nothing in a Go test or a bundler check can tell whether a forked PostgreSQL
// compiled to WebAssembly actually creates a property graph and evaluates a
// GRAPH_TABLE query inside a Web Worker. That is the claim the whole change
// rests on, so it is asserted here — against the exact build docs/package.json
// pins, served from a preview-shaped subpath, by a server that sets no
// cross-origin-isolation headers.

import { test, expect } from '@playwright/test'

/** The wasm and filesystem bundle a visitor must never download unasked. */
const RUNTIME_ASSET = /pglite[-.][^/]*\.(wasm|data)$/

/** Record every request for a runtime asset. The returned array is live. */
function watchRuntimeRequests(page) {
  const seen = []
  page.on('request', (req) => {
    if (RUNTIME_ASSET.test(new URL(req.url()).pathname)) seen.push(req.url())
  })
  return seen
}

/** Load the page and wait for the WASM module to boot. */
async function boot(page) {
  await page.goto('index.html')
  await expect(page.locator('#boot')).toHaveClass(/ok/, { timeout: 60_000 })
}

/**
 * Switch tabs the way a reader does. Inactive panels are `hidden`, so a control
 * inside one cannot be clicked until its tab is selected.
 */
async function activateTab(page, id) {
  await page.locator(`ga-tabs#tabs button[data-id="${id}"]`).click()
}

/** The Run control for a scenario. */
const runButton = (page, scenario) =>
  page.locator(`ga-button.exec[data-scenario="${scenario}"]`)

/** Press Run and wait for the worker's answer to replace the progress note. */
async function runScenario(page, scenario, panel) {
  await runButton(page, scenario).click()
  await expect(page.locator(panel)).toContainText('Starting PostgreSQL')
  await expect(page.locator(panel)).not.toContainText('Starting PostgreSQL')
}

/** The rendered result table as { columns, rows }. */
async function readTable(page, panel) {
  return page.locator(`${panel} table.result-table`).evaluate((table) => ({
    columns: [...table.querySelectorAll('thead th')].map((th) => th.textContent),
    rows: [...table.querySelectorAll('tbody tr')].map((tr) =>
      [...tr.querySelectorAll('td')].map((td) => td.textContent)),
  }))
}

/**
 * A CodeMirror pane's document. `.cm-content`'s textContent would run the lines
 * together, and CodeMirror uses a non-breaking space where a trailing space
 * would collapse, so both are undone here.
 */
async function readEditor(page, id) {
  const lines = await page.locator(`#${id} .cm-line`).allTextContents()
  return lines.join('\n').replace(/ /g, ' ')
}

/** Replace a CodeMirror pane's whole document. */
async function setEditor(page, id, text) {
  await page.locator(`#${id} .cm-content`).click()
  await page.keyboard.press('ControlOrMeta+a')
  await page.keyboard.insertText(text)
}

// The worked example with one field's physical column renamed. The query still
// compiles — it filters on v0.full_name — but the seed still inserts into
// `name`, so the seed no longer applies to the schema.
const RENAMED_COLUMN_SDL = `type Person @node(label: "person") {
  id: ID!
  name: String! @column(name: "full_name")
  email: String
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

// The same example with a column type PostgreSQL does not have. @column(type:)
// is raw SQL by design, so this reaches the database exactly as written and is
// rejected there rather than by gopgql.
const BAD_TYPE_SDL = `type Person @node(label: "person") {
  id: ID!
  name: String!
  email: String @column(type: "nosuchtype")
  follows: [Person!]! @relationship(type: "follows", direction: OUT)
  followedBy: [Person!]! @relationship(type: "follows", direction: IN)
                         @hasInverse(field: "follows")
}`

test('the page boots, generates, and fetches no runtime asset', async ({ page }) => {
  const runtimeRequests = watchRuntimeRequests(page)
  await boot(page)

  // Generating is the whole of what the page did before this change, and it
  // must still cost a visitor nothing.
  await page.locator('ga-button.run[data-scenario="traversal"]').click()
  await expect(page.locator('#t-status')).toHaveText('generated')

  expect(runtimeRequests,
    'loading and generating must not fetch the PostgreSQL runtime').toEqual([])

  // The fork is linked -sUSE_PTHREADS=0, so the site needs no COOP/COEP. The
  // suite's server sets none; this is what keeps that claim honest.
  expect(await page.evaluate(() => globalThis.crossOriginIsolated)).toBe(false)
})

test('a GRAPH_TABLE query executes in the browser and renders rows', async ({ page }) => {
  const runtimeRequests = watchRuntimeRequests(page)
  await boot(page)

  await runScenario(page, 'traversal', '#t-result')

  const { columns, rows } = await readTable(page, '#t-result')

  // The compiled query projects id and name for each of the four vertices in
  // the three-hop chain.
  expect(columns).toEqual(
    ['v0_k', 'v0_c0', 'v1_k', 'v1_c0', 'v2_k', 'v2_c0', 'v3_k', 'v3_c0'])

  // The seed's chain is Alice -> Bob -> Carol -> Dave, filtered to Alice by the
  // bind parameter, so exactly one path satisfies the pattern.
  expect(rows).toHaveLength(1)
  expect(rows[0][1]).toBe('Alice')
  expect(rows[0][3]).toBe('Bob')
  expect(rows[0][5]).toBe('Carol')
  expect(rows[0][7]).toBe('Dave')

  await expect(page.locator('#t-exec-status')).toHaveText('1 row')

  // Only now is the runtime fetched.
  expect(runtimeRequests.length,
    'pressing Run is what downloads the runtime').toBeGreaterThan(0)

  // The panel names what produced the rows. The fork emits no `PGlite x.y.z`
  // suffix, so nothing may require one — this asserts the PostgreSQL version
  // and prints the whole string for the record.
  const version = await page.locator('#t-result .result-note.provenance').textContent()
  console.log(`SELECT version() -> ${version}`)
  expect(version).toContain('PostgreSQL 19')
  expect(version).toContain('wasm32')
})

test('the SQL that ran is the SQL on the page, with values bound not interpolated', async ({ page }) => {
  await boot(page)
  const displayed = await readEditor(page, 't-sql')

  await runScenario(page, 'traversal', '#t-result')

  const executed = await page.evaluate(() => globalThis.gopgqlLastRun)
  expect(executed.sql).toBe(displayed)
  // The value travels beside the SQL, not inside it.
  expect(executed.args).toEqual(['Alice'])
  expect(executed.sql).toContain('$1')
  expect(executed.sql).not.toContain('Alice')
  // And it is the plain generated DDL that is applied, not the goose document.
  expect(executed.ddl).toContain('CREATE PROPERTY GRAPH')
  expect(executed.ddl).not.toContain('+goose')
})

test('bind values keep their kinds across the WASM boundary', async ({ page }) => {
  await boot(page)

  // Compile runs on the main thread, so this exercises the Go -> JS crossing
  // directly: each value has to arrive as itself, not as its printed form.
  const args = await page.evaluate(() => {
    const sdl = `type Thing @node(label: "thing") {
  id: ID!
  name: String!
  count: Int!
  ratio: Float!
  active: Boolean!
}`
    const out = globalThis.gopgqlCompile(
      sdl,
      '{ things(name: $s, count: $i, ratio: $f, active: $b) { name } }',
      '{"s":"Chain","i":7,"f":1.5,"b":true}')
    return { error: out.error, parsed: JSON.parse(out.args) }
  })

  expect(args.error).toBe('')
  expect(args.parsed).toEqual(['Chain', 7, 1.5, true])
  expect(typeof args.parsed[0]).toBe('string')
  expect(typeof args.parsed[1]).toBe('number')
  expect(typeof args.parsed[2]).toBe('number')
  expect(typeof args.parsed[3]).toBe('boolean')
})

const runnable = [
  { scenario: 'multipattern', tab: 'multipattern', panel: '#p-result', status: '#p-exec-status' },
  { scenario: 'directives', tab: 'directives', panel: '#c-result', status: '#c-exec-status' },
  { scenario: 'constraints', tab: 'constraints', panel: '#k-result', status: '#k-exec-status' },
  { scenario: 'interfaces', tab: 'interfaces', panel: '#i-result', status: '#i-exec-status' },
]

for (const { scenario, tab, panel, status } of runnable) {
  test(`${scenario} runs and returns rows`, async ({ page }) => {
    await boot(page)
    await activateTab(page, tab)
    await runScenario(page, scenario, panel)
    const { rows } = await readTable(page, panel)
    expect(rows.length).toBeGreaterThan(0)
    await expect(page.locator(status)).toHaveText(/row/)
  })
}

test('a refused query offers no Run, and offers it once it compiles', async ({ page }) => {
  await boot(page)
  await activateTab(page, 'depth')

  // The Depth tab's default query is refused for exceeding the ceiling, so
  // there is no SQL to send and the control says so.
  await expect(runButton(page, 'depth')).toHaveAttribute('disabled', '')
  await expect(page.locator('#d-status')).toHaveText('rejected, as designed')

  // Raise the ceiling and the same query compiles — and becomes runnable.
  await page.locator('#d-max').fill('4')
  await expect(page.locator('#d-status')).toHaveText(/compiled within MaxDepth 4/)
  await expect(runButton(page, 'depth')).not.toHaveAttribute('disabled', '')

  await runScenario(page, 'depth', '#d-result')
  const { rows } = await readTable(page, '#d-result')
  expect(rows.length).toBeGreaterThan(0)
})

test('each run starts from an empty database, and the runtime loads once', async ({ page }) => {
  const runtimeRequests = watchRuntimeRequests(page)
  await boot(page)

  await runScenario(page, 'traversal', '#t-result')
  const first = await readTable(page, '#t-result')
  const afterFirstRun = runtimeRequests.length
  expect(afterFirstRun).toBeGreaterThan(0)

  // A different schema through the same worker. If state survived, the two
  // schemas would be sharing a database neither of them expects.
  await activateTab(page, 'directives')
  await runScenario(page, 'directives', '#c-result')
  expect((await readTable(page, '#c-result')).rows.length).toBeGreaterThan(0)

  // And the first scenario again: a second CREATE TABLE persons against a
  // surviving database would fail with "already exists".
  await activateTab(page, 'traversal')
  await runScenario(page, 'traversal', '#t-result')
  expect(await readTable(page, '#t-result')).toEqual(first)

  expect(runtimeRequests.length,
    'the runtime is fetched once and reused').toBe(afterFirstRun)
})

test('a seed that no longer fits the schema does not abort the run', async ({ page }) => {
  await boot(page)

  await setEditor(page, 't-sdl', RENAMED_COLUMN_SDL)
  await expect(page.locator('#t-status')).toHaveText('generated')

  await runScenario(page, 'traversal', '#t-result')

  // The seed failure is reported as itself — a fixture that no longer applies,
  // not a broken page...
  await expect(page.locator('#t-result .result-note.warn')).toBeVisible()
  await expect(page.locator('#t-result .result-error')).toContainText('name')
  // ...and the query still ran, honestly returning nothing.
  await expect(page.locator('#t-result')).toContainText('returned no rows')
  await expect(page.locator('#t-result .result-note.error')).toHaveCount(0)
})

test('a database error is shown verbatim, under the step that raised it', async ({ page }) => {
  await boot(page)

  await setEditor(page, 't-sdl', BAD_TYPE_SDL)
  await expect(page.locator('#t-status')).toHaveText('generated')

  await runScenario(page, 'traversal', '#t-result')

  await expect(page.locator('#t-result .result-note.error'))
    .toHaveText(/Applying the generated schema failed/)
  // PostgreSQL's own words, not a paraphrase.
  await expect(page.locator('#t-result .result-error')).toContainText('nosuchtype')
  await expect(page.locator('#t-exec-status')).toHaveText('schema rejected')

  // A failed run leaves the generated panes alone.
  await expect(page.locator('#t-sql')).toContainText('GRAPH_TABLE')
})

test('nothing is persisted', async ({ page }) => {
  await boot(page)
  await runScenario(page, 'traversal', '#t-result')

  const stored = await page.evaluate(async () => {
    const dbs = (await indexedDB.databases?.()) ?? []
    let opfs = []
    try {
      const root = await navigator.storage.getDirectory()
      for await (const name of root.keys()) opfs.push(name)
    } catch {
      opfs = []
    }
    return { indexedDB: dbs.map((d) => d.name), opfs }
  })

  expect(stored.indexedDB, 'the database is in memory only').toEqual([])
  expect(stored.opfs, 'no OPFS backing store').toEqual([])
})
