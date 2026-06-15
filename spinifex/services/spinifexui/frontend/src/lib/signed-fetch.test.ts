import { afterEach, describe, expect, it, vi } from "vitest"

import type { SessionCredentials } from "./auth"
import { isStaleCredentialsError } from "./auth-error"
import { SignedFetchError, signedFetch } from "./signed-fetch"

const credentials: SessionCredentials = {
  accessKeyId: "ASIAIOSFODNN7EXAMPLE",
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  sessionToken: "FwoGZXIvYXdzEBYaD-session-token",
  expiration: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
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
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it("emits X-Amz-Security-Token when the session token is present", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response("{}", { status: 200 }))
    vi.stubGlobal("fetch", fetchMock)

    await signedFetch({ action: "GetVersion", credentials })

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

    const err = await signedFetch({
      action: "GetVersion",
      credentials,
    }).catch((error: unknown) => error)

    expect(err).toBeInstanceOf(SignedFetchError)
    expect(err).toMatchObject({ name: "InvalidClientTokenId", status: 403 })
    expect(isStaleCredentialsError(err)).toBeTruthy()
  })

  it("falls back to a generic name when the body has no <Code>", async () => {
    stubFetch("internal failure", 500)

    const err = await signedFetch({
      action: "GetVersion",
      credentials,
    }).catch((error: unknown) => error)

    expect(err).toBeInstanceOf(SignedFetchError)
    expect(err).toMatchObject({ name: "SignedFetchError", status: 500 })
    expect(isStaleCredentialsError(err)).toBeFalsy()
  })
})
