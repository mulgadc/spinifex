import { describe, expect, it } from "vitest"

import { isStaleCredentialsError } from "./auth-error"

// Build an SDK-shaped error: an Error whose `name` is the AWS error code.
function sdkError(name: string): Error {
  const err = new Error(`${name} occurred`)
  err.name = name
  return err
}

describe("isStaleCredentialsError", () => {
  const authCodes = [
    "InvalidClientTokenId",
    "SignatureDoesNotMatch",
    "AuthFailure",
    "MissingAuthenticationToken",
    "IncompleteSignature",
    "UnrecognizedClientException",
    "ExpiredToken",
    "ExpiredTokenException",
    "InvalidAccessKeyId",
  ]

  it.each(authCodes)("returns true for auth code %s", (code) => {
    expect(isStaleCredentialsError(sdkError(code))).toBeTruthy()
  })

  it("matches plain objects carrying the code on name", () => {
    expect(
      isStaleCredentialsError({ name: "InvalidClientTokenId" }),
    ).toBeTruthy()
  })

  const nonAuthCases: [string, unknown][] = [
    ["AccessDenied authorization denial", sdkError("AccessDenied")],
    ["UnauthorizedOperation denial", sdkError("UnauthorizedOperation")],
    ["ValidationError", sdkError("ValidationError")],
    ["InvalidParameterValue", sdkError("InvalidParameterValue")],
    ["NotFound", sdkError("InvalidInstanceID.NotFound")],
    ["plain Error", new Error("boom")],
    ["network failure", new TypeError("Failed to fetch")],
    ["null", null],
    ["undefined", undefined],
    ["string", "InvalidClientTokenId"],
    ["object without name", { code: "InvalidClientTokenId" }],
    ["non-string name", { name: 403 }],
  ]

  it.each(nonAuthCases)("returns false for %s", (_label, value) => {
    expect(isStaleCredentialsError(value)).toBeFalsy()
  })
})
