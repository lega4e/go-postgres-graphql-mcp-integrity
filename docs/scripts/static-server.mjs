// A zero-dependency static server for the built site, used by the browser
// suite.
//
// Two things about it are load-bearing rather than incidental:
//
//   1. It sets **no** cross-origin-isolation headers. The fork is linked with
//      -sUSE_PTHREADS=0, so SharedArrayBuffer is unused and no COOP/COEP is
//      needed; serving the suite from a plain server is how that claim is
//      tested rather than assumed. A server that quietly set them would make
//      the suite pass on hosting the real site cannot have.
//
//   2. It can mount the site under a subpath, so the suite can exercise a
//      `pr-preview/pr-<N>/` deployment shape — where the worker URL and the
//      runtime's asset URLs are the two things `base: './'` can get wrong.
//
// Usage: node scripts/static-server.mjs [--port 4173] [--base /some/prefix/]

import { createServer } from 'node:http'
import { readFile, stat } from 'node:fs/promises'
import { join, extname, normalize, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const docsDir = join(dirname(fileURLToPath(import.meta.url)), '..')
const distDir = join(docsDir, 'dist')

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`)
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback
}

const port = Number(arg('port', process.env.PORT ?? 4173))
// Normalised to a leading and trailing slash, so '/' and '/pr/x/' both work.
let base = arg('base', process.env.BASE_PATH ?? '/')
if (!base.startsWith('/')) base = `/${base}`
if (!base.endsWith('/')) base = `${base}/`

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  // WebAssembly.instantiateStreaming refuses anything else.
  '.wasm': 'application/wasm',
  '.data': 'application/octet-stream',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.ico': 'image/x-icon',
  '.map': 'application/json; charset=utf-8',
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://localhost:${port}`)
  let pathname = decodeURIComponent(url.pathname)

  if (!pathname.startsWith(base)) {
    res.writeHead(404, { 'content-type': 'text/plain' })
    res.end(`not under ${base}`)
    return
  }
  pathname = `/${pathname.slice(base.length)}`
  if (pathname.endsWith('/')) pathname += 'index.html'

  // normalize plus the prefix check below is what keeps ../ inside dist.
  const file = join(distDir, normalize(pathname))
  if (!file.startsWith(distDir)) {
    res.writeHead(403, { 'content-type': 'text/plain' })
    res.end('forbidden')
    return
  }

  try {
    const info = await stat(file)
    if (!info.isFile()) throw new Error('not a file')
    const body = await readFile(file)
    res.writeHead(200, {
      'content-type': TYPES[extname(file)] ?? 'application/octet-stream',
      'content-length': body.length,
      // Deliberately no Cross-Origin-Opener-Policy and no
      // Cross-Origin-Embedder-Policy. See the note at the top.
    })
    res.end(body)
  } catch {
    res.writeHead(404, { 'content-type': 'text/plain' })
    res.end(`not found: ${pathname}`)
  }
})

server.listen(port, () => {
  console.log(`serving ${distDir} at http://localhost:${port}${base}`)
})
