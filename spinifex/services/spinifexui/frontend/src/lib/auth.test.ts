import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { AwsCredentials } from "./auth"

const validCreds: AwsCredentials = {
  accessKeyId: "AKIAIOSFODNN7EXAMPLE",
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
}

let getCredentials: typeof import("./auth").getCredentials
let setCredentials: typeof import("./auth").setCredentials
let clearCredentials: typeof import("./auth").clearCredentials

async function loadAuth() {
  vi.resetModules()
  const auth = await import("./auth")
  getCredentials = auth.getCredentials
  setCredentials = auth.setCredentials
  clearCredentials = auth.clearCredentials
}

describe("setCredentials", () => {
  beforeEach(loadAuth)
  afterEach(() => {
    localStorage.clear()
  })

  it("stores credentials in localStorage", () => {
    setCredentials(validCreds)
    const stored = localStorage.getItem("spinifex:v1:aws-credentials")
    expect(JSON.parse(stored ?? "")).toStrictEqual(validCreds)
  })

  it("caches credentials in memory", () => {
    setCredentials(validCreds)
    // Clear localStorage to prove it reads from cache
    localStorage.clear()
    expect(getCredentials()).toStrictEqual(validCreds)
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

  it("reads from localStorage on first call", () => {
    localStorage.setItem(
      "spinifex:v1:aws-credentials",
      JSON.stringify(validCreds),
    )
    expect(getCredentials()).toStrictEqual(validCreds)
  })

  it("returns cached value on subsequent calls", () => {
    setCredentials(validCreds)
    localStorage.clear()
    // Should still return from cache
    expect(getCredentials()).toStrictEqual(validCreds)
    expect(getCredentials()).toStrictEqual(validCreds)
  })

  it("returns null for invalid JSON in localStorage", () => {
    localStorage.setItem("spinifex:v1:aws-credentials", "not-json")
    expect(getCredentials()).toBeNull()
  })

  it("returns null when stored data fails schema validation", () => {
    localStorage.setItem(
      "spinifex:v1:aws-credentials",
      JSON.stringify({ accessKeyId: "short", secretAccessKey: "" }),
    )
    expect(getCredentials()).toBeNull()
  })

  it("returns null when stored object is missing fields", () => {
    localStorage.setItem(
      "spinifex:v1:aws-credentials",
      JSON.stringify({ accessKeyId: "AKIAIOSFODNN7EXAMPLE" }),
    )
    expect(getCredentials()).toBeNull()
  })
})

describe("clearCredentials", () => {
  beforeEach(loadAuth)
  afterEach(() => {
    localStorage.clear()
  })

  it("removes credentials from localStorage", () => {
    setCredentials(validCreds)
    clearCredentials()
    expect(localStorage.getItem("spinifex:v1:aws-credentials")).toBeNull()
  })

  it("clears the in-memory cache", () => {
    setCredentials(validCreds)
    clearCredentials()
    expect(getCredentials()).toBeNull()
  })
})
