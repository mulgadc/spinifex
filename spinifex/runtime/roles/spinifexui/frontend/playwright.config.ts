import { defineConfig, devices } from "@playwright/test"

// Two servers: Vite dev for behaviour, and the built dist under vite preview,
// where the e2e suite adds the console's CSP so inline script or style fails.
const DEV_URL = "https://localhost:3101"
const PREVIEW_URL = "https://localhost:3102"

export default defineConfig({
  testDir: "e2e",
  testMatch: "**/*.e2e.ts",
  snapshotPathTemplate: "e2e/__screenshots__/{testFileName}/{arg}{ext}",
  fullyParallel: true,
  timeout: 30_000,
  globalTimeout: 10 * 60_000,
  forbidOnly: Boolean(process.env.CI),
  reporter: process.env.CI ? "line" : "list",
  use: {
    ...devices["Desktop Chrome"],
    ignoreHTTPSErrors: true,
    reducedMotion: "reduce",
    // Pinned so dates in screenshots do not move with the host's zone.
    locale: "en-AU",
    timezoneId: "UTC",
    trace: "retain-on-failure",
  },
  expect: {
    toHaveScreenshot: { maxDiffPixelRatio: 0.002, animations: "disabled" },
  },
  projects: [
    {
      name: "dev",
      testIgnore: "**/csp.e2e.ts",
      use: { baseURL: DEV_URL },
    },
    {
      name: "preview",
      testMatch: "**/csp.e2e.ts",
      use: { baseURL: PREVIEW_URL },
    },
  ],
  // Vite is run directly: under `pnpm exec` the stop signal never reaches it,
  // so the run waits out globalTimeout and leaves the server orphaned.
  webServer: [
    {
      command: "node_modules/.bin/vite --port 3101 --strictPort",
      url: DEV_URL,
      ignoreHTTPSErrors: true,
      reuseExistingServer: !process.env.CI,
    },
    {
      command: "node_modules/.bin/vite preview --port 3102 --strictPort",
      url: PREVIEW_URL,
      ignoreHTTPSErrors: true,
      reuseExistingServer: !process.env.CI,
    },
  ],
})
