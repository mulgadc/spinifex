import { cleanup } from "@testing-library/react"
import "@testing-library/jest-dom/vitest"
import { afterEach, vi } from "vitest"

import { loadClusterConfig } from "@/lib/cluster-config"

afterEach(() => {
  cleanup()
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
