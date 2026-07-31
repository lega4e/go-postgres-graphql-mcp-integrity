// The PostgreSQL side of the playground.
//
// This module owns the only reference to the forked PGlite build in the whole
// site. It runs in a dedicated Web Worker because `initdb` on a fresh in-memory
// database takes seconds, and the page has to stay usable while it happens —
// that is the entire reason a worker exists here, and the protocol below is
// kept small so it stays obvious.
//
// It is deliberately *not* `@electric-sql/pglite/worker`. `PGliteWorker` exists
// to elect a leader among tabs sharing one **persisted** database; this
// playground is ephemeral and in-memory, so two tabs sharing a database is the
// wrong semantics, not a feature (design D4).
//
// The protocol is three messages:
//
//   in   { type: 'run', id, ddl, seed, sql, args }
//   out  { type: 'result', id, version, steps: { schema, seed, query } }
//   out  { type: 'error', id, message }
//
// `result` reports each step separately so the page can name the one that
// failed; `error` is for a failure that is not a step — the runtime itself
// failing to load or instantiate. Everything crossing the boundary is text,
// plain arrays and plain values. The Go module lives on the main thread and its
// linear memory is never reachable from here (design D5).

// db is the database the *previous* run created. Each run discards it and
// starts from an empty one, so an edited schema cannot land on top of the
// tables the last run made (design D3).
let db = null

// serverVersion is read once per database and reported with the result, so a
// reader can see which PostgreSQL produced the rows in front of them.
let serverVersion = ''

/**
 * Load the forked PGlite build.
 *
 * This is the one place in the site that names the module, and it is a dynamic
 * import inside the run path — so the ~15 MB of wasm and filesystem bundle is
 * fetched when someone asks for a query to be executed, and never before. The
 * worker itself is only constructed on the first Run, so this module's own code
 * is not on the boot path either.
 */
async function loadPGlite() {
  const { PGlite } = await import('@electric-sql/pglite')
  return PGlite
}

/**
 * Build a fresh, empty, in-memory database.
 *
 * `new PGlite()` with no `dataDir` keeps everything in memory: no IndexedDB, no
 * OPFS, nothing that survives a reload. The previous database is closed first,
 * so repeated runs do not accumulate half-applied schemas.
 */
async function freshDatabase() {
  if (db) {
    const closing = db
    db = null
    try {
      await closing.close()
    } catch {
      // A database that will not close is already unusable; the run that
      // follows builds a new one regardless, and reporting this would name a
      // step the reader did not ask for.
    }
  }
  const PGlite = await loadPGlite()
  db = new PGlite()
  await db.waitReady
  const version = await db.query('SELECT version()')
  // The fork's version string carries no `PGlite x.y.z` suffix — nothing here
  // may assume one is present.
  serverVersion = version.rows?.[0]?.version ?? ''
  return db
}

/**
 * Reduce a value PostgreSQL returned to something plainly serialisable.
 *
 * postMessage structured-clones what it is given, and a Date or a typed array
 * would survive that — but the page renders text, and keeping the boundary to
 * strings, numbers, booleans and null means the worker's output is exactly as
 * inspectable as it looks. Anything else is rendered here, once, rather than in
 * three places on the page.
 */
function plainValue(value) {
  if (value === null || value === undefined) return null
  const kind = typeof value
  if (kind === 'string' || kind === 'number' || kind === 'boolean') return value
  if (value instanceof Date) return value.toISOString()
  if (kind === 'bigint') return value.toString()
  return JSON.stringify(value)
}

/** The message PostgreSQL raised, without page chrome wrapped around it. */
function messageOf(err) {
  if (!err) return 'unknown error'
  return String(err.message ?? err)
}

/**
 * Apply one multi-statement document — the generated DDL, or a seed.
 *
 * `exec` is the multi-statement entry point; `query` is the single-statement,
 * parameter-binding one. The generated schema is several statements, so it has
 * to be the former.
 */
async function applyDocument(sql) {
  if (!sql || !sql.trim()) return { ok: true, skipped: true }
  try {
    await db.exec(sql)
    return { ok: true }
  } catch (err) {
    return { ok: false, error: messageOf(err) }
  }
}

/**
 * Execute the compiled query with its bind values.
 *
 * `args` are the ordered values behind $1, $2, … exactly as the compiler
 * emitted them; nothing is substituted into the SQL text. `rowMode: 'array'`
 * keeps rows positional, so the column names come from `fields` and two columns
 * that happen to share a name cannot collapse into one.
 */
async function runQuery(sql, args) {
  try {
    const out = await db.query(sql, args ?? [], { rowMode: 'array' })
    return {
      ok: true,
      columns: (out.fields ?? []).map((f) => f.name),
      rows: (out.rows ?? []).map((row) => row.map(plainValue)),
    }
  } catch (err) {
    return { ok: false, error: messageOf(err) }
  }
}

/**
 * One run: a fresh database, the generated schema, the seed, then the query.
 *
 * A seed failure does **not** abort the run. The seed is a fixture bound to an
 * *example* schema, so a reader who edited the schema should see the seed fail
 * and the query still execute — returning no rows, honestly, rather than being
 * told the page is broken (design D7).
 *
 * A schema failure does abort: every later step would fail with "relation does
 * not exist", which buries the one error that actually explains the run.
 */
async function run({ ddl, seed, sql, args }) {
  await freshDatabase()

  const steps = { schema: null, seed: null, query: null }

  steps.schema = await applyDocument(ddl)
  if (!steps.schema.ok) return { steps }

  steps.seed = await applyDocument(seed)
  steps.query = await runQuery(sql, args)
  return { steps }
}

self.addEventListener('message', async (event) => {
  const request = event.data
  if (!request || request.type !== 'run') return
  try {
    const { steps } = await run(request)
    self.postMessage({
      type: 'result',
      id: request.id,
      version: serverVersion,
      steps,
    })
  } catch (err) {
    // Reached when the runtime itself could not be loaded or a database could
    // not be created — not when PostgreSQL rejected a statement, which is a
    // step outcome above.
    self.postMessage({ type: 'error', id: request.id, message: messageOf(err) })
  }
})
