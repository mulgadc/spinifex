import { defineConfig, devices } from "@playwright/test"

// The console served by a real cluster, with no API interception. The mocked
// suite in playwright.config.ts cannot see a defect that lives in what the
// control plane actually sends, which is how an idle production cluster came
// to report no compute available.
//
// Pointed at a cluster by SPX_CONSOLE_URL, with SPX_ACCESS_KEY_ID and
// SPX_SECRET_ACCESS_KEY for an account that may launch instances. It creates
// and terminates real resources, so it never runs by default and starts no
// server of its own.
const CONSOLE_URL = process.env.SPX_CONSOLE_URL

export default defineConfig({
  testDir: "e2e/live",
  testMatch: "**/*.live.ts",
  // Serial: the suite launches guests and a parallel run would race the
  // cluster's capacity against itself.
  workers: 1,
  timeout: 10 * 60_000,
  globalTimeout: 40 * 60_000,
  forbidOnly: Boolean(process.env.CI),
  reporter: process.env.CI ? "line" : "list",
  retries: 0,
  use: {
    ...devices["Desktop Chrome"],
    baseURL: CONSOLE_URL,
    // The cluster CA is its own, and the console is reached by node name.
    ignoreHTTPSErrors: true,
    reducedMotion: "reduce",
    locale: "en-AU",
    timezoneId: "UTC",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },
})
