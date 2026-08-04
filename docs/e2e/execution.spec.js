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

/**
 * The shaped GraphQL response the panel leads with, parsed.
 *
 * This is the whole point of a Run: a GraphQL query went in, and this is the
 * response gopgql built from the rows PostgreSQL returned. It is produced by
 * the Go module — shape.Rows, the same function the integration suites assert
 * on — so what is read here is the library's output, not the page's rendering
 * of it.
 */
async function readResponse(page, panel) {
  return JSON.parse(await page.locator(`${panel} pre.result-json`).textContent())
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

/**
 * The data SQL a scenario's pane was filled with, read from the WASM module.
 *
 * Deliberately not read back off the page: CodeMirror only renders the lines in
 * its viewport, so `readEditor` on a pane taller than its box returns part of
 * the document. Appending to that and writing it back would silently drop the
 * rest of the seed — and the test would still pass, against data nobody meant
 * to insert. The module's own export is the same text the page seeded the pane
 * with, and all of it.
 */
async function seedData(page, exportName) {
  return page.evaluate((name) => globalThis[name], exportName)
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

  // The panel names what produced the rows: PostgreSQL's own version string,
  // and the pinned fork build it came from. The fork emits no `PGlite x.y.z`
  // suffix, so nothing may require one — the whole string is printed for the
  // record and only the PostgreSQL version is asserted.
  const provenance = await page.locator('#t-result .result-note.provenance').allTextContents()
  console.log(`provenance -> ${provenance.join(' | ')}`)
  expect(provenance[0]).toContain('PostgreSQL 19')
  expect(provenance[0]).toContain('wasm32')
  expect(provenance[1]).toContain('postgres-pglite pglite-wasm-19beta2.1')
  expect(provenance[1]).toContain('@electric-sql/pglite@0.5.4-pg19beta2')
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

// --- the response, and changing the data it is built from --------------------
//
// Executing is only two thirds of gopgql's job. The rows PostgreSQL returns are
// flat and carry the compiler's own column names; what a GraphQL client asked
// for is a nested document. These tests assert the last leg — and then assert
// that a reader who changes the data gets a different one.

test('the flat rows are shaped into the nested GraphQL response', async ({ page }) => {
  await boot(page)
  await runScenario(page, 'traversal', '#t-result')

  // The three-hop query nests three levels deep, and the response mirrors the
  // query's own shape rather than the result set's: GraphQL response keys, not
  // v0_c0.
  expect(await readResponse(page, '#t-result')).toEqual({
    persons: [{
      name: 'Alice',
      follows: [{ name: 'Bob', follows: [{ name: 'Carol', follows: [{ name: 'Dave' }] }] }],
    }],
  })

  // The flat rows are still there, one disclosure away: they are the evidence
  // the response was built from, and the page does not ask to be believed.
  expect((await readTable(page, '#t-result')).rows).toHaveLength(1)

  // No `{"data": …}` envelope. gopgql shapes a payload; it is not a server
  // answering a request, and an envelope it never produces would be the page's
  // one invented value.
  const shaped = JSON.parse(await page.evaluate(() => globalThis.gopgqlLastRun.shaped))
  expect(shaped).not.toHaveProperty('data')
  expect(Object.keys(shaped)).toEqual(['persons'])
})

test('an UPDATE the reader writes changes the next response', async ({ page }) => {
  await boot(page)
  await runScenario(page, 'traversal', '#t-result')
  expect((await readResponse(page, '#t-result')).persons[0].follows[0].name).toBe('Bob')

  // What the Data pane is for. A property graph is a read-only view, so this is
  // SQL rather than a GraphQL mutation — and it runs against the same in-memory
  // database the query is about to read.
  await expect(page.locator('#t-data'),
    'the pane starts with the seed INSERTs').toContainText('INSERT INTO persons')

  const seeded = await seedData(page, 'gopgqlExampleSeed')
  await setEditor(page, 't-data',
    `${seeded}\n\nUPDATE persons SET name = 'Robert' WHERE name = 'Bob';`)

  await runScenario(page, 'traversal', '#t-result')

  const after = await readResponse(page, '#t-result')
  expect(after.persons[0].follows[0].name).toBe('Robert')
  expect(after.persons[0].name).toBe('Alice')
  await expect(page.locator('#t-result .result-note.warn')).toHaveCount(0)
})

test('an INSERT fans the result out, and shaping still carries one parent', async ({ page }) => {
  await boot(page)

  // A second outgoing edge from Alice gives the pattern a second three-hop path
  // (Alice -> Carol -> Dave -> Erin), so PostgreSQL returns two rows that repeat
  // Alice. Deduplicating her is exactly what shaping is for, and it is only
  // visible once there is a fan-out to collapse.
  const seeded = await seedData(page, 'gopgqlExampleSeed')
  await setEditor(page, 't-data', `${seeded}\n\nINSERT INTO follows (source_id, target_id) VALUES\n` +
    `  ('a0000000-0000-4000-8000-000000000001', 'a0000000-0000-4000-8000-000000000003');`)

  await runScenario(page, 'traversal', '#t-result')

  expect((await readTable(page, '#t-result')).rows,
    'two paths satisfy the pattern').toHaveLength(2)

  const response = await readResponse(page, '#t-result')
  expect(response.persons, 'both rows repeat Alice; the response carries her once').toHaveLength(1)
  // Row order is PostgreSQL's to choose, so the set is asserted, not the order.
  expect(response.persons[0].follows.map((f) => f.name).sort()).toEqual(['Bob', 'Carol'])
})

test('data SQL the database refuses is reported, and the query still answers', async ({ page }) => {
  await boot(page)

  await setEditor(page, 't-data', `UPDATE persons SET nosuchcolumn = 1;`)

  await runScenario(page, 'traversal', '#t-result')

  // The reader's own mistake, named as theirs and shown in PostgreSQL's words.
  await expect(page.locator('#t-result .result-note.warn')).toBeVisible()
  await expect(page.locator('#t-result .result-error')).toContainText('nosuchcolumn')

  // And the run is not abandoned: the query still executed against the empty
  // schema, and its response is an empty list rather than an error or a blank
  // panel.
  await expect(page.locator('#t-result')).toContainText('returned no rows')
  expect(await readResponse(page, '#t-result')).toEqual({ persons: [] })
  await expect(page.locator('#t-result .result-note.error')).toHaveCount(0)
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

// --- the Shaping tab -------------------------------------------------------
//
// Every test above runs one statement. The Shaping tab runs two — the same
// GraphQL query compiled under each result-shaping strategy — and its claim is
// that they produce the same response. That claim is proven in Go against a real
// postgres:19beta2 by test/parity; what can only be proven here is that the page
// actually runs both and actually compares them, in a browser, against the
// pinned wasm build.

test('the two shaping strategies agree, live', async ({ page }) => {
  await boot(page)
  await activateTab(page, 'shaping')

  // Both statements are compiled by the same generation, whichever one the
  // toggle is showing. Flipping it changes the SQL on screen and nothing else:
  // the bind parameters are part of the MATCH, which neither strategy touches.
  await expect(page.locator('#s-sql')).toContainText('GRAPH_TABLE')
  const goSideSQL = await page.locator('#s-sql').textContent()
  const params = await page.locator('#s-params').textContent()

  await runScenario(page, 'shaping', '#s-result')

  await expect(page.locator('#s-result .result-note.ok'))
    .toHaveText(/produced the same response — \d+ identical bytes\./)
  await expect(page.locator('#s-exec-status')).toHaveText('responses identical')

  // The two responses are equal, and they are the response the seed describes —
  // not the same empty document twice, which would satisfy equality and prove
  // nothing.
  const responses = await page.locator('#s-result pre.result-json').allTextContents()
  expect(responses, 'one response per strategy').toHaveLength(2)
  const [goSide, sqlSide] = responses.map((text) => JSON.parse(text))
  expect(goSide).toEqual(sqlSide)
  expect(goSide).toEqual({
    persons: [{
      name: 'Alice',
      follows: [{ name: 'Bob' }, { name: 'Carol' }],
      followedBy: [{ name: 'Dave' }, { name: 'Erin' }],
    }],
  })

  // And the result sets behind them are genuinely different, which is the
  // reason two strategies exist. A seed that stopped fanning out would leave
  // the equality above true and the tab meaningless.
  const outcome = await page.evaluate(() => globalThis.gopgqlLastParity)
  expect(outcome.identical).toBe(true)
  expect(outcome.goRows, 'Alice has two follows and two followers, so 2×2').toBe(4)
  expect(outcome.sqlRows, 'PostgreSQL assembles the whole response into one row').toBe(1)

  // Both statements ran against one database. Two runs would each build a fresh
  // one, and a difference could then always be blamed on the data.
  const ran = await page.evaluate(() => globalThis.gopgqlLastRun)
  expect(ran.queries).toHaveLength(2)
  expect(ran.queries.map((q) => q.key)).toEqual(['goSide', 'sqlSide'])
  expect(ran.sql, 'the statement on the page is one of the two that ran').toBe(goSideSQL)

  // Flipping the toggle shows the other strategy's statement — and only that.
  await page.locator('#s-strategy button[data-id="sql-side"]').click()
  await expect(page.locator('#s-sql')).toContainText('json_build_object')
  await expect(page.locator('#s-params')).toHaveText(params)
})

test('the shaping tab reports a rejected statement rather than a mismatch', async ({ page }) => {
  await boot(page)
  await activateTab(page, 'shaping')

  // A schema PostgreSQL refuses. Both statements still compile, so a panel that
  // only ever reported "identical" or "different" would have to pick one of
  // them here — and either would blame the strategies for something neither of
  // them did.
  await setEditor(page, 's-sdl', BAD_TYPE_SDL)
  await expect(page.locator('#s-status')).toHaveText(/generated/)

  await runScenario(page, 'shaping', '#s-result')

  await expect(page.locator('#s-result .result-note.error').first())
    .toHaveText(/Applying the generated schema failed/)
  await expect(page.locator('#s-exec-status')).toHaveText('schema rejected')
})
