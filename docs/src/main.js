import './style.css'

const sdlEl = document.getElementById('sdl')
const gqlEl = document.getElementById('gql')
const ddlEl = document.getElementById('ddl')
const queryEl = document.getElementById('query')
const runEl = document.getElementById('run')
const statusEl = document.getElementById('status')

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

function render() {
  const result = globalThis.gopgqlGenerate(sdlEl.value, gqlEl.value)
  if (result.error) {
    ddlEl.textContent = ''
    queryEl.textContent = ''
    statusEl.textContent = result.error
    statusEl.className = 'status error'
    return
  }
  ddlEl.textContent = result.migration
  queryEl.textContent = result.sql
  statusEl.textContent = 'generated'
  statusEl.className = 'status ok'
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
    runEl.disabled = false
    statusEl.textContent = 'ready'
    statusEl.className = 'status ok'
    render()
  } catch (err) {
    statusEl.textContent = String(err)
    statusEl.className = 'status error'
  }
}

runEl.addEventListener('click', render)
boot()
