import { defineConfig, devices } from '@playwright/test'

// The site is served from a **preview-shaped subpath**, not the site root.
//
// `base: './'` in vite.config.js is what makes the deployed site work under
// gh-pages' pr-preview/pr-<N>/ directories, and the two things it can get wrong
// are exactly the two things this change adds: the worker's URL and the
// runtime's wasm/data asset URLs. Running the whole suite under a subpath means
// a regression there fails every test rather than none of them.
const BASE_PATH = '/gopgql/pr-preview/pr-0/'
const PORT = 4173

export default defineConfig({
  testDir: '.',
  // initdb on a fresh in-memory database takes seconds, and the first run also
  // downloads ~15 MB. A CI runner is slower than that sounds.
  timeout: 180_000,
  expect: { timeout: 120_000 },
  // Serial: every worker would download and instantiate its own PostgreSQL.
  workers: 1,
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: 0,
  reporter: process.env.CI ? [['list'], ['github']] : [['list']],
  use: {
    baseURL: `http://127.0.0.1:${PORT}${BASE_PATH}`,
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    // No COOP/COEP headers: the fork is built -sUSE_PTHREADS=0, and the suite
    // asserts crossOriginIsolated is false to keep that honest.
    command: `node ../scripts/static-server.mjs --port ${PORT} --base ${BASE_PATH}`,
    url: `http://127.0.0.1:${PORT}${BASE_PATH}`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
})
