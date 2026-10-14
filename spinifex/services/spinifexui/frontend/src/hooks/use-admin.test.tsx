import { QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

import type { SessionCredentials } from "@/lib/auth"
import type { SignedFetchOptions } from "@/lib/signed-fetch"
import { createTestQueryClient } from "@/test/elbv2-integration"

const mockGetCredentials = vi.fn<() => SessionCredentials | null>()
const mockStsSend = vi.fn()
const mockSignedFetch = vi.fn<(opts: SignedFetchOptions) => Promise<unknown>>()

vi.mock("@/lib/auth", () => ({
  getCredentials: () => mockGetCredentials(),
}))

vi.mock("@/lib/sts", () => ({
  getStsClient: () => ({ send: mockStsSend }),
}))

vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: async (opts: SignedFetchOptions) => await mockSignedFetch(opts),
  signedAdminFetch: vi.fn(),
}))

vi.mock("@aws-sdk/client-sts", () => ({
  // Constructable stub: `new GetCallerIdentityCommand({})` must not throw.
  GetCallerIdentityCommand: vi.fn(),
}))

import { useAdmin } from "./use-admin"

const sessionCreds = {
  accessKeyId: "ASIAIOSFODNN7EXAMPLE",
  secretAccessKey: "secret",
  sessionToken: "token",
  expiration: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
}

const versionInfo = {
  version: "v1.0.0",
  commit: "abc1234",
  os: "linux",
  arch: "amd64",
  license: "open-source",
}

function renderUseAdmin() {
  const queryClient = createTestQueryClient()
  function wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    )
  }
  return renderHook(() => useAdmin(), { wrapper })
}

describe("useAdmin", () => {
  beforeEach(() => {
    mockGetCredentials.mockReset()
    mockStsSend.mockReset()
    mockSignedFetch.mockReset()
  })

  it("marks the admin account and derives the user name from the ARN", async () => {
    mockGetCredentials.mockReturnValue(sessionCreds)
    mockStsSend.mockResolvedValue({
      Account: "000000000001",
      Arn: "arn:aws:iam::000000000001:user/admin",
    })
    mockSignedFetch.mockResolvedValue(versionInfo)

    const { result } = renderUseAdmin()

    await waitFor(() => {
      expect(result.current.version).not.toBeNull()
    })

    expect(result.current.isAdmin).toBeTruthy()
    expect(result.current.accountId).toBe("000000000001")
    expect(result.current.userName).toBe("admin")
    expect(result.current.version).toStrictEqual(versionInfo)
    expect(result.current.license).toBe("open-source")
    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "GetVersion",
      credentials: sessionCreds,
    })
  })

  it("treats a non-admin account as non-admin and skips the version fetch", async () => {
    mockGetCredentials.mockReturnValue(sessionCreds)
    mockStsSend.mockResolvedValue({
      Account: "000000000002",
      Arn: "arn:aws:iam::000000000002:user/alice",
    })

    const { result } = renderUseAdmin()

    await waitFor(() => {
      expect(result.current.accountId).toBe("000000000002")
    })

    expect(result.current.isAdmin).toBeFalsy()
    expect(result.current.userName).toBe("alice")
    expect(result.current.version).toBeNull()
    expect(mockSignedFetch).not.toHaveBeenCalled()
  })

  it("yields a null user name when the ARN is not a user ARN", async () => {
    mockGetCredentials.mockReturnValue(sessionCreds)
    mockStsSend.mockResolvedValue({
      Account: "000000000001",
      Arn: "arn:aws:iam::000000000001:root",
    })
    mockSignedFetch.mockResolvedValue(versionInfo)

    const { result } = renderUseAdmin()

    await waitFor(() => {
      expect(result.current.isAdmin).toBeTruthy()
    })

    expect(result.current.userName).toBeNull()
  })

  it("is non-admin when the identity lookup fails", async () => {
    mockGetCredentials.mockReturnValue(sessionCreds)
    mockStsSend.mockRejectedValue(new Error("gateway unreachable"))

    const { result } = renderUseAdmin()

    await waitFor(() => {
      expect(mockStsSend).toHaveBeenCalled()
    })

    expect(result.current.isAdmin).toBeFalsy()
    expect(result.current.accountId).toBeNull()
    expect(mockSignedFetch).not.toHaveBeenCalled()
  })

  it("skips the identity lookup when unauthenticated", () => {
    mockGetCredentials.mockReturnValue(null)

    const { result } = renderUseAdmin()

    expect(result.current.isAdmin).toBeFalsy()
    expect(result.current.accountId).toBeNull()
    expect(mockStsSend).not.toHaveBeenCalled()
  })
})
