import { afterEach, describe, expect, it, vi } from "vitest"

import type { AwsCredentials } from "./auth"
import { isStaleCredentialsError } from "./auth-error"
import { SignedFetchError, signedFetch } from "./signed-fetch"

const credentials: AwsCredentials = {
  accessKeyId: "AKIAIOSFODNN7EXAMPLE",
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
}

function stubFetch(body: string, status: number) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(new Response(body, { status })),
  )
}

describe("signedFetch error handling", () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it("throws a typed error carrying the AWS code from XML <Code>", async () => {
    stubFetch(
      `<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>InvalidClientTokenId</Code><Message>The X.509 certificate or credentials provided do not exist in our records.</Message></Error></Errors><RequestID>req-1</RequestID></Response>`,
      403,
    )

    const err = await signedFetch({
      action: "GetCallerIdentity",
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
