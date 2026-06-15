import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@aws-sdk/client-sts", () => ({
  // Mocked SDK constructors must be constructable with `new`; a class works
  // where a vi.fn(arrow) would throw "not a constructor".
  STSClient: class {
    send = mockSend
    middlewareStack = { add: vi.fn() }
  },
  GetSessionTokenCommand: vi.fn(),
}))

import { GetSessionTokenCommand } from "@aws-sdk/client-sts"

import { exchangeForSession } from "./sts"

const longLived = {
  accessKeyId: "AKIAIOSFODNN7EXAMPLE",
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
}

describe("exchangeForSession", () => {
  it("maps a GetSessionToken response to SessionCredentials", async () => {
    const expiration = new Date(Date.now() + 12 * 60 * 60 * 1000)
    mockSend.mockResolvedValue({
      Credentials: {
        AccessKeyId: "ASIAIOSFODNN7EXAMPLE",
        SecretAccessKey: "session-secret",
        SessionToken: "session-token",
        Expiration: expiration,
      },
    })

    const result = await exchangeForSession(longLived)

    expect(result).toStrictEqual({
      accessKeyId: "ASIAIOSFODNN7EXAMPLE",
      secretAccessKey: "session-secret",
      sessionToken: "session-token",
      expiration: expiration.toISOString(),
    })
  })

  it("requests a 12 hour session duration", async () => {
    mockSend.mockResolvedValue({
      Credentials: {
        AccessKeyId: "ASIA",
        SecretAccessKey: "s",
        SessionToken: "t",
        Expiration: new Date(Date.now() + 60_000),
      },
    })

    await exchangeForSession(longLived)

    expect(GetSessionTokenCommand).toHaveBeenCalledWith({
      DurationSeconds: 43_200,
    })
  })

  it("throws when the response omits credentials", async () => {
    mockSend.mockResolvedValue({})
    await expect(exchangeForSession(longLived)).rejects.toThrow(
      "incomplete credentials",
    )
  })
})
