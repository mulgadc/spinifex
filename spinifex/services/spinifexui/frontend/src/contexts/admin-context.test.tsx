import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import type { SessionCredentials } from "@/lib/auth"

const mockGetCredentials = vi.fn<() => SessionCredentials | null>()
const mockStsSend = vi.fn()
const mockSignedFetch = vi.fn<(opts: unknown) => Promise<unknown>>()

vi.mock("@/lib/auth", () => ({
  getCredentials: () => mockGetCredentials(),
}))

vi.mock("@/lib/sts", () => ({
  getStsClient: () => ({ send: mockStsSend }),
}))

vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: async (opts: unknown) => mockSignedFetch(opts),
}))

vi.mock("@aws-sdk/client-sts", () => ({
  // Constructable stub: `new GetCallerIdentityCommand({})` must not throw.
  GetCallerIdentityCommand: vi.fn(),
}))

import { AdminProvider, useAdmin } from "./admin-context"

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

function wrapper({ children }: { children: ReactNode }) {
  return <AdminProvider>{children}</AdminProvider>
}

describe("AdminProvider", () => {
  it("marks the admin account and derives the user name from the ARN", async () => {
    mockGetCredentials.mockReturnValue(sessionCreds)
    mockStsSend.mockResolvedValue({
      Account: "000000000001",
      Arn: "arn:aws:iam::000000000001:user/admin",
    })
    mockSignedFetch.mockResolvedValue(versionInfo)

    const { result } = renderHook(() => useAdmin(), { wrapper })

    await waitFor(() => {
      expect(result.current.loading).toBeFalsy()
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

    const { result } = renderHook(() => useAdmin(), { wrapper })

    await waitFor(() => {
      expect(result.current.loading).toBeFalsy()
    })

    expect(result.current.isAdmin).toBeFalsy()
    expect(result.current.accountId).toBe("000000000002")
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

    const { result } = renderHook(() => useAdmin(), { wrapper })

    await waitFor(() => {
      expect(result.current.loading).toBeFalsy()
    })

    expect(result.current.isAdmin).toBeTruthy()
    expect(result.current.userName).toBeNull()
  })

  it("stops loading without detection when unauthenticated", async () => {
    mockGetCredentials.mockReturnValue(null)

    const { result } = renderHook(() => useAdmin(), { wrapper })

    await waitFor(() => {
      expect(result.current.loading).toBeFalsy()
    })

    expect(result.current.isAdmin).toBeFalsy()
    expect(result.current.accountId).toBeNull()
    expect(mockStsSend).not.toHaveBeenCalled()
  })
})
