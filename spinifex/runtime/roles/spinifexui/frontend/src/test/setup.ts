import type * as TestingLibraryReact from "@testing-library/react"
import { cleanup } from "@testing-library/react"
import "@testing-library/jest-dom/vitest"
import { afterEach, vi } from "vitest"

import { loadClusterConfig } from "@/lib/cluster-config"

afterEach(() => {
  cleanup()
})

// waitFor wakes on DOM mutations, otherwise it polls every 50ms. renderHook
// tests settle without touching the DOM, so each one pays a full tick.
vi.mock("@testing-library/react", async (importOriginal) => {
  const actual = await importOriginal<typeof TestingLibraryReact>()
  return {
    ...actual,
    waitFor: async (
      callback: Parameters<typeof actual.waitFor>[0],
      options?: Parameters<typeof actual.waitFor>[1],
    ) => await actual.waitFor(callback, { interval: 1, ...options }),
  }
})

// Components sign requests with the region the server reports, which the app
// normally loads before rendering. Seed it through the real code path so every
// component test has one without stubbing fetch itself.
vi.stubGlobal(
  "fetch",
  vi.fn().mockResolvedValue({
    ok: true,
    json: vi.fn().mockResolvedValue({ region: "ap-southeast-2" }),
  }),
)
await loadClusterConfig()
vi.unstubAllGlobals()
