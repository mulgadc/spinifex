import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

// Keep the real awsCredentialsSchema (the shape check is part of what we test),
// mock only the side-effecting helpers.
vi.mock("./sts", () => ({
  exchangeForSession: vi.fn(),
}))
vi.mock("./awsClient", () => ({
  clearClients: vi.fn(),
}))
vi.mock("./auth", async (orig) => ({
  ...(await orig<typeof import("./auth")>()),
  setSessionCredentials: vi.fn(),
}))

import { setSessionCredentials } from "./auth"
import { clearClients } from "./awsClient"
import { startConsoleHandoff } from "./console-handoff"
import { exchangeForSession } from "./sts"

const GOOD = {
  accessKeyId: "AKIAIOSFODNN7EXAMPLE",
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
}
const SESSION = {
  accessKeyId: "ASIAIOSFODNN7EXAMPLE",
  secretAccessKey: "sessionsecret",
  sessionToken: "token",
  expiration: new Date(Date.now() + 3_600_000).toISOString(),
}

let opener: { postMessage: ReturnType<typeof vi.fn> }
let assign: ReturnType<typeof vi.fn>

/** Dispatch a message event with an explicit source (jsdom won't set it). */
function postToConsole(opts: {
  data: unknown
  origin: string
  source: unknown
}) {
  const ev = new MessageEvent("message", {
    data: opts.data,
    origin: opts.origin,
  })
  Object.defineProperty(ev, "source", {
    value: opts.source,
    configurable: true,
  })
  window.dispatchEvent(ev)
}

const tick = async () => new Promise((r) => setTimeout(r, 0))

beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(exchangeForSession).mockResolvedValue(SESSION)
  opener = { postMessage: vi.fn() }
  Object.defineProperty(window, "opener", {
    value: opener,
    configurable: true,
    writable: true,
  })
  assign = vi.fn()
  Object.defineProperty(window, "location", {
    value: { ...window.location, assign },
    configurable: true,
    writable: true,
  })
})

afterEach(() => {
  Object.defineProperty(window, "opener", {
    value: null,
    configurable: true,
    writable: true,
  })
})

describe("startConsoleHandoff", () => {
  it("announces readiness to the allowed opener origins on start", () => {
    startConsoleHandoff()
    const targets = opener.postMessage.mock.calls.map((c) => c[1])
    expect(opener.postMessage).toHaveBeenCalledWith(
      { type: "spx-handoff-ready" },
      "https://mulgadc.com",
    )
    expect(targets).toContain("https://staging.mulgadc.com")
    // Never a wildcard target.
    expect(targets).not.toContain("*")
  })

  it("exchanges credentials from an allowed origin and signs in", async () => {
    startConsoleHandoff()
    postToConsole({
      data: { type: "spx-handoff-creds", ...GOOD },
      origin: "https://mulgadc.com",
      source: opener,
    })
    await tick()
    expect(exchangeForSession).toHaveBeenCalledWith(GOOD)
    expect(setSessionCredentials).toHaveBeenCalledWith(SESSION)
    expect(clearClients).toHaveBeenCalled()
    expect(assign).toHaveBeenCalledWith("/")
  })

  it("ignores credentials from a disallowed origin", async () => {
    startConsoleHandoff()
    postToConsole({
      data: { type: "spx-handoff-creds", ...GOOD },
      origin: "https://evil.example",
      source: opener,
    })
    await tick()
    expect(exchangeForSession).not.toHaveBeenCalled()
  })

  it("ignores messages whose source is not the opener", async () => {
    startConsoleHandoff()
    postToConsole({
      data: { type: "spx-handoff-creds", ...GOOD },
      origin: "https://mulgadc.com",
      source: {},
    })
    await tick()
    expect(exchangeForSession).not.toHaveBeenCalled()
  })

  it("ignores the wrong message type", async () => {
    startConsoleHandoff()
    postToConsole({
      data: { type: "something-else", ...GOOD },
      origin: "https://mulgadc.com",
      source: opener,
    })
    await tick()
    expect(exchangeForSession).not.toHaveBeenCalled()
  })

  it("ignores a malformed payload (missing secret)", async () => {
    startConsoleHandoff()
    postToConsole({
      data: { type: "spx-handoff-creds", accessKeyId: GOOD.accessKeyId },
      origin: "https://mulgadc.com",
      source: opener,
    })
    await tick()
    expect(exchangeForSession).not.toHaveBeenCalled()
  })

  it("calls onFailure and does not navigate when STS rejects", async () => {
    vi.mocked(exchangeForSession).mockRejectedValueOnce(new Error("bad creds"))
    const onFailure = vi.fn()
    startConsoleHandoff({ onFailure })
    postToConsole({
      data: { type: "spx-handoff-creds", ...GOOD },
      origin: "https://mulgadc.com",
      source: opener,
    })
    await tick()
    expect(onFailure).toHaveBeenCalled()
    expect(assign).not.toHaveBeenCalled()
  })

  it("is a no-op with no opener", () => {
    Object.defineProperty(window, "opener", {
      value: null,
      configurable: true,
      writable: true,
    })
    const stop = startConsoleHandoff()
    expect(stop).toBeTypeOf("function")
    stop()
  })

  it("only acts once, then the listener is removed", async () => {
    startConsoleHandoff()
    postToConsole({
      data: { type: "spx-handoff-creds", ...GOOD },
      origin: "https://mulgadc.com",
      source: opener,
    })
    await tick()
    postToConsole({
      data: { type: "spx-handoff-creds", ...GOOD },
      origin: "https://mulgadc.com",
      source: opener,
    })
    await tick()
    expect(exchangeForSession).toHaveBeenCalledTimes(1)
  })
})
