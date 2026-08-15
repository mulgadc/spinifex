import { afterEach, describe, expect, it, vi } from "vitest"

import { getRegion, loadClusterConfig } from "./cluster-config"

function mockFetch(response: Partial<Response>) {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response))
}

describe("loadClusterConfig", () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it("makes the served region readable", async () => {
    mockFetch({
      ok: true,
      json: async () => ({ region: "us-west-1" }),
    })

    await loadClusterConfig()

    expect(getRegion()).toBe("us-west-1")
  })

  it("rejects a failed request", async () => {
    mockFetch({ ok: false, status: 500 })

    await expect(loadClusterConfig()).rejects.toThrow("HTTP 500")
  })

  // Silently accepting an empty region would sign requests with "undefined",
  // which the gateway rejects as an opaque credential error.
  it("rejects a response with no region", async () => {
    mockFetch({ ok: true, json: async () => ({}) })

    await expect(loadClusterConfig()).rejects.toThrow(
      "did not include a region",
    )
  })

  it("rejects a response that is not an object", async () => {
    mockFetch({ ok: true, json: async () => "us-west-1" })

    await expect(loadClusterConfig()).rejects.toThrow("was not an object")
  })
})

describe("getRegion", () => {
  it("throws before the config has been loaded", async () => {
    vi.resetModules()
    const fresh = await import("./cluster-config")

    expect(() => fresh.getRegion()).toThrow("has not been loaded")
  })
})
