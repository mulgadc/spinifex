import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  clearCredentials,
  type SessionCredentials,
  setSessionCredentials,
} from "./auth"
import { isStaleCredentialsError } from "./auth-error"
import {
  signedAdminFetch,
  SignedFetchError,
  signedFetch,
  signedProxyXmlFetch,
} from "./signed-fetch"

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

describe("signedProxyXmlFetch", () => {
  beforeEach(() => {
    setSessionCredentials(credentials)
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    clearCredentials()
  })

  const PATH = "/2013-04-01/hostedzone/Z1/activation"

  it("sends a GET with no body or content type, signed for the named service", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response("<Root><Value>1</Value></Root>"))

    vi.stubGlobal("fetch", fetchMock)

    const doc = await signedProxyXmlFetch({
      label: "GetHostedZoneActivation",
      service: "route53",
      method: "GET",
      path: PATH,
    })

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toMatch(
      /\/proxy\/awsgw\/2013-04-01\/hostedzone\/Z1\/activation$/,
    )
    expect(init.method).toBe("GET")
    expect(init.body).toBeUndefined()
    const headers = init.headers as Record<string, string>
    expect(findHeader(headers, "content-type")).toBeUndefined()
    expect(findHeader(headers, "authorization")).toContain(
      "/route53/aws4_request",
    )
    expect(doc.documentElement.localName).toBe("Root")
    expect(doc.documentElement.textContent).toBe("1")
  })

  it("sends a POST with an empty body when none is given", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response("<Root/>"))

    vi.stubGlobal("fetch", fetchMock)

    await signedProxyXmlFetch({
      label: "VerifyHostedZoneDelegation",
      service: "route53",
      path: PATH,
    })

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit
    expect(init.method).toBe("POST")
    expect(init.body).toBeUndefined()
  })

  it("throws the Route 53 error code from an ErrorResponse body", async () => {
    stubFetch(
      `<?xml version="1.0"?><ErrorResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><Error><Type>Sender</Type><Code>NoSuchHostedZone</Code><Message>No hosted zone found with ID: Z1</Message></Error><RequestId>r</RequestId></ErrorResponse>`,
      404,
    )

    const err = await rejection(
      signedProxyXmlFetch({
        label: "GetHostedZoneActivation",
        service: "route53",
        method: "GET",
        path: PATH,
      }),
    )

    expect(err).toBeInstanceOf(SignedFetchError)
    expect(err).toMatchObject({ name: "NoSuchHostedZone", status: 404 })
    expect(err.message).toContain("No hosted zone found with ID: Z1")
  })

  it("rejects a response that is not XML", async () => {
    stubFetch("<unclosed", 200)

    const err = await rejection(
      signedProxyXmlFetch({
        label: "GetHostedZoneActivation",
        service: "route53",
        method: "GET",
        path: PATH,
      }),
    )

    expect(err).toBeInstanceOf(SignedFetchError)
    expect(err.message).toContain("not valid XML")
  })
})
