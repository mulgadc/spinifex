import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  DEFAULT_DOCS_URL,
  docsUrl,
  getBranding,
  loadBranding,
  productName,
  resetBrandingForTest,
} from "./branding"

interface BrandingBody {
  enabled: boolean
  revision?: number
  productName?: string
  docsUrl?: string
  stylesheet?: string
  logo?: string
  favicon?: string
}

function mockBranding(body: BrandingBody) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({ ok: true, json: async () => body }),
  )
}

describe("loadBranding", () => {
  beforeEach(() => {
    document.head.innerHTML = '<link rel="icon" href="/favicon.svg">'
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.useRealTimers()
    resetBrandingForTest()
  })

  it("keeps the built-in brand when the request fails", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("offline")))

    await loadBranding()

    expect(productName()).toBe("Spinifex")
    expect(docsUrl()).toBe(DEFAULT_DOCS_URL)
  })

  it("keeps the built-in brand when the gateway answers with an error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: false, status: 502 }),
    )

    await loadBranding()

    expect(getBranding().enabled).toBeFalsy()
  })

  it("keeps the built-in brand when branding is not enabled", async () => {
    mockBranding({ enabled: false, productName: "Acme Cloud" })

    await loadBranding()

    expect(productName()).toBe("Spinifex")
  })

  it("applies the product name, the title and a hidden Docs link", async () => {
    mockBranding({
      enabled: true,
      revision: 4,
      productName: "Acme Cloud",
      docsUrl: "",
    })

    await loadBranding()

    expect(productName()).toBe("Acme Cloud")
    expect(docsUrl()).toBeNull()
  })

  it("never uses a Docs link that is not https", async () => {
    mockBranding({ enabled: true, docsUrl: "http://docs.acme.example" })

    await loadBranding()

    expect(docsUrl()).toBe(DEFAULT_DOCS_URL)
  })

  it("replaces the favicon with the versioned one", async () => {
    mockBranding({ enabled: true, revision: 4, favicon: "favicon?rev=4" })

    await loadBranding()

    const icons = [...document.querySelectorAll('link[rel="icon"]')]
    expect(icons.map((l) => l.getAttribute("href"))).toStrictEqual([
      "/branding/favicon?rev=4",
    ])
  })

  it("ignores an asset path that is not a revisioned name", async () => {
    mockBranding({
      enabled: true,
      logo: "//evil.example/x.svg",
      stylesheet: "../x.css",
    })

    await loadBranding()

    expect(getBranding().logo).toBeUndefined()
    expect(document.querySelector('link[rel="stylesheet"]')).toBeNull()
  })

  // The stylesheet may never arrive; the budget, not the stylesheet, decides
  // when the console renders.
  it("stops waiting for a stalled stylesheet when the budget ends", async () => {
    vi.useFakeTimers()
    mockBranding({
      enabled: true,
      revision: 4,
      stylesheet: "branding.css?rev=4",
    })

    let settled = false

    const loading = (async () => {
      await loadBranding(2000)
      settled = true
    })()

    await vi.advanceTimersByTimeAsync(1999)
    expect(settled).toBeFalsy()
    expect(
      document.querySelector('link[rel="stylesheet"]')?.getAttribute("href"),
    ).toBe("/branding/branding.css?rev=4")

    await vi.advanceTimersByTimeAsync(1)
    await loading
    expect(settled).toBeTruthy()
  })

  it("aborts a stalled branding request when the budget ends", async () => {
    vi.useFakeTimers()
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit) => {
        // oxlint-disable-next-line promise/avoid-new -- a request that never answers until it is aborted
        await new Promise((_resolve, reject) => {
          init.signal?.addEventListener("abort", () => {
            reject(new DOMException("timed out", "TimeoutError"))
          })
        })
      }),
    )

    const loading = loadBranding(2000)
    await vi.advanceTimersByTimeAsync(2000)
    await loading

    expect(getBranding().enabled).toBeFalsy()
  })
})
