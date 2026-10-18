import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { SessionCredentials } from "./auth"

const STORAGE_KEY = "spinifex:v2:aws-session"

function sessionCreds(
  overrides: Partial<SessionCredentials> = {},
): SessionCredentials {
  return {
    accessKeyId: "ASIAIOSFODNN7EXAMPLE",
    secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
    sessionToken: "FwoGZXIvYXdzEBYaD-session-token",
    expiration: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
    ...overrides,
  }
}

let getCredentials: typeof import("./auth").getCredentials
let setSessionCredentials: typeof import("./auth").setSessionCredentials
let clearCredentials: typeof import("./auth").clearCredentials
let onSessionChangedInOtherTab: typeof import("./auth").onSessionChangedInOtherTab

async function loadAuth() {
  vi.resetModules()

  const auth = await import("./auth")

  ;({
    getCredentials,
    setSessionCredentials,
    clearCredentials,
    onSessionChangedInOtherTab,
  } = auth)
}

describe("setSessionCredentials", () => {
  beforeEach(loadAuth)
  afterEach(() => {
    localStorage.clear()
  })

  it("stores session credentials in localStorage", () => {
    const creds = sessionCreds()
    setSessionCredentials(creds)
    const stored = localStorage.getItem(STORAGE_KEY)
    expect(JSON.parse(stored ?? "")).toStrictEqual(creds)
  })

  it("caches credentials in memory", () => {
    const creds = sessionCreds()
    setSessionCredentials(creds)
    // Clear localStorage to prove it reads from cache
    localStorage.clear()
    expect(getCredentials()).toStrictEqual(creds)
  })
})

describe("getCredentials", () => {
  beforeEach(loadAuth)
  afterEach(() => {
    localStorage.clear()
  })

  it("returns null when nothing is stored", () => {
    expect(getCredentials()).toBeNull()
  })

  it("round-trips sessionToken and expiration from localStorage", () => {
    const creds = sessionCreds()
    localStorage.setItem(STORAGE_KEY, JSON.stringify(creds))
    expect(getCredentials()).toStrictEqual(creds)
  })

  it("returns cached value on subsequent calls", () => {
    const creds = sessionCreds()
    setSessionCredentials(creds)
    localStorage.clear()
    expect(getCredentials()).toStrictEqual(creds)
    expect(getCredentials()).toStrictEqual(creds)
  })

  it("returns null for invalid JSON in localStorage", () => {
    localStorage.setItem(STORAGE_KEY, "not-json")
    expect(getCredentials()).toBeNull()
  })

  it("returns null when the session token is missing", () => {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({
        accessKeyId: "ASIAIOSFODNN7EXAMPLE",
        secretAccessKey: "secret",
        expiration: new Date(Date.now() + 60_000).toISOString(),
      }),
    )
    expect(getCredentials()).toBeNull()
  })

  it("returns null and clears storage when the session has expired", () => {
    const expired = sessionCreds({
      expiration: new Date(Date.now() - 1000).toISOString(),
    })

    localStorage.setItem(STORAGE_KEY, JSON.stringify(expired))
    expect(getCredentials()).toBeNull()
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it("returns null when the expiration is unparseable", () => {
    const bad = sessionCreds({ expiration: "not-a-date" })
    localStorage.setItem(STORAGE_KEY, JSON.stringify(bad))
    expect(getCredentials()).toBeNull()
  })
})

describe("clearCredentials", () => {
  beforeEach(loadAuth)
  afterEach(() => {
    localStorage.clear()
  })

  it("removes credentials from localStorage", () => {
    setSessionCredentials(sessionCreds())
    clearCredentials()
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull()
  })

  it("clears the in-memory cache", () => {
    setSessionCredentials(sessionCreds())
    clearCredentials()
    expect(getCredentials()).toBeNull()
  })
})

// What the browser fires in this tab when another tab writes localStorage.
function storageEvent(key: string | null) {
  window.dispatchEvent(new StorageEvent("storage", { key }))
}

describe("onSessionChangedInOtherTab", () => {
  beforeEach(loadAuth)
  afterEach(() => {
    localStorage.clear()
  })

  it("drops the cached session when another tab signs out", () => {
    setSessionCredentials(sessionCreds())
    const handler = vi.fn()
    const stop = onSessionChangedInOtherTab(handler)

    localStorage.removeItem(STORAGE_KEY)
    storageEvent(STORAGE_KEY)
    stop()

    expect(handler).toHaveBeenCalledOnce()
    expect(getCredentials()).toBeNull()
  })

  it("picks up the session another tab signed in with", () => {
    setSessionCredentials(sessionCreds({ accessKeyId: "ASIAOLDSESSION" }))
    const stop = onSessionChangedInOtherTab(vi.fn())

    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify(sessionCreds({ accessKeyId: "ASIANEWSESSION" })),
    )
    storageEvent(STORAGE_KEY)
    stop()

    expect(getCredentials()?.accessKeyId).toBe("ASIANEWSESSION")
  })

  it("reacts when another tab clears all of localStorage", () => {
    const handler = vi.fn()
    const stop = onSessionChangedInOtherTab(handler)

    storageEvent(null)
    stop()

    expect(handler).toHaveBeenCalledOnce()
  })

  it("ignores changes to other keys", () => {
    setSessionCredentials(sessionCreds())
    const handler = vi.fn()
    const stop = onSessionChangedInOtherTab(handler)

    storageEvent("spinifex-ui-theme")
    stop()

    expect(handler).not.toHaveBeenCalled()
    expect(getCredentials()).not.toBeNull()
  })
})
