import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  clearCredentials,
  type SessionCredentials,
  setSessionCredentials,
} from "./auth"
import { isStaleCredentialsError } from "./auth-error"
import { signedAdminFetch, SignedFetchError, signedFetch } from "./signed-fetch"

const credentials: SessionCredentials = {
  accessKeyId: "ASIAIOSFODNN7EXAMPLE",
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  sessionToken: "FwoGZXIvYXdzEBYaD-session-token",
  expiration: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
}

// Captures the rejection so the test can assert on the error object itself,
// and fails loudly if the request unexpectedly resolves.
async function rejection(promise: Promise<unknown>): Promise<Error> {
  try {
    await promise
  } catch (error) {
    return error as Error
  }

  throw new Error("expected the request to reject")
}

function stubFetch(body: string, status: number) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(new Response(body, { status })),
  )
}

function findHeader(
  headers: Record<string, string>,
  name: string,
): string | undefined {
  const lower = name.toLowerCase()

  return Object.entries(headers).find(
    ([key]) => key.toLowerCase() === lower,
  )?.[1]
}

describe("signedFetch", () => {
  beforeEach(() => {
    setSessionCredentials(credentials)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    clearCredentials()
  })

  it("POSTs the Action= form to the gateway root", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }))

    vi.stubGlobal("fetch", fetchMock)

    await signedFetch({ action: "GetVersion" })

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toMatch(/\/proxy\/awsgw\/$/)
    expect(init.body).toBe("Action=GetVersion")
  })

  it("emits X-Amz-Security-Token when the session token is present", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }))

    vi.stubGlobal("fetch", fetchMock)

    await signedFetch({ action: "GetVersion" })

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit
    const headers = init.headers as Record<string, string>
    expect(findHeader(headers, "x-amz-security-token")).toBe(
      credentials.sessionToken,
    )
  })

  it("throws a typed error carrying the AWS code from XML <Code>", async () => {
    stubFetch(
      `<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>InvalidClientTokenId</Code><Message>The X.509 certificate or credentials provided do not exist in our records.</Message></Error></Errors><RequestID>req-1</RequestID></Response>`,
      403,
    )

    const err = await rejection(signedFetch({ action: "GetVersion" }))

    expect(err).toBeInstanceOf(SignedFetchError)
    expect(err).toMatchObject({ name: "InvalidClientTokenId", status: 403 })
    expect(isStaleCredentialsError(err)).toBeTruthy()
  })

  it("falls back to a generic name when the body has no <Code>", async () => {
    stubFetch("internal failure", 500)

    const err = await rejection(signedFetch({ action: "GetVersion" }))

    expect(err).toBeInstanceOf(SignedFetchError)
    expect(err).toMatchObject({ name: "SignedFetchError", status: 500 })
    expect(isStaleCredentialsError(err)).toBeFalsy()
  })

  it("rejects without sending when there is no session", async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal("fetch", fetchMock)
    clearCredentials()

    await expect(signedFetch({ action: "GetVersion" })).rejects.toThrow(
      "Not authenticated",
    )
    expect(fetchMock).not.toHaveBeenCalled()
  })
})

describe("signedAdminFetch", () => {
  beforeEach(() => {
    setSessionCredentials(credentials)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    clearCredentials()
  })

  it("POSTs a JSON body to /proxy/awsgw/admin/<Method>", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response('{"accounts":[],"count":0}', { status: 200 }),
      )

    vi.stubGlobal("fetch", fetchMock)

    await signedAdminFetch({ method: "ListAccounts", body: {} })

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toContain("/proxy/awsgw/admin/ListAccounts")
    expect(init.method).toBe("POST")
    expect(init.body).toBe("{}")
    const headers = init.headers as Record<string, string>
    expect(findHeader(headers, "content-type")).toBe("application/json")
  })

  it("defaults a missing body to an empty JSON object", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }))

    vi.stubGlobal("fetch", fetchMock)

    await signedAdminFetch({ method: "ListAccounts" })

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit
    expect(init.body).toBe("{}")
  })

  it("decodes the /admin JSON error envelope", async () => {
    stubFetch(
      '{"error":{"code":"AccessDenied","message":"not the super-admin account"},"requestId":"req-1"}',
      403,
    )

    const err = await rejection(signedAdminFetch({ method: "ListAccounts" }))

    expect(err).toBeInstanceOf(SignedFetchError)
    expect(err).toMatchObject({ name: "AccessDenied", status: 403 })
    expect(err.message).toContain("not the super-admin account")
  })

  it("falls back to a generic name when the body is not the envelope", async () => {
    stubFetch("gateway unavailable", 503)

    const err = await rejection(signedAdminFetch({ method: "ListAccounts" }))

    expect(err).toBeInstanceOf(SignedFetchError)
    expect(err).toMatchObject({ name: "SignedFetchError", status: 503 })
  })
})
