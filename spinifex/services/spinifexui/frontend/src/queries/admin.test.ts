import { describe, expect, it, vi } from "vitest"

vi.mock("@/lib/auth", () => ({
  getCredentials: vi.fn(),
}))

vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: vi.fn(),
}))

import { getCredentials } from "@/lib/auth"
import { signedFetch } from "@/lib/signed-fetch"

import {
  adminNodesQueryOptions,
  adminStorageStatusQueryOptions,
  adminVMsQueryOptions,
} from "./admin"

const mockGetCredentials = vi.mocked(getCredentials)
const mockSignedFetch = vi.mocked(signedFetch)

function getQueryFn(opts: {
  queryFn?: unknown
}): (ctx: never) => Promise<unknown> {
  if (typeof opts.queryFn !== "function") {
    throw new TypeError("queryFn is not defined")
  }
  return opts.queryFn as (ctx: never) => Promise<unknown>
}

describe("adminNodesQueryOptions", () => {
  it("has the correct query key", () => {
    expect(adminNodesQueryOptions.queryKey).toStrictEqual(["admin", "nodes"])
  })

  it("throws when not authenticated", async () => {
    mockGetCredentials.mockReturnValue(null)
    await expect(
      getQueryFn(adminNodesQueryOptions)({} as never),
    ).rejects.toThrow("Not authenticated")
  })

  it("calls signedFetch with GetNodes action", async () => {
    const creds = {
      accessKeyId: "ASIAak",
      secretAccessKey: "sk",
      sessionToken: "token",
      expiration: new Date(Date.now() + 60_000).toISOString(),
    }
    mockGetCredentials.mockReturnValue(creds)
    mockSignedFetch.mockResolvedValue({ nodes: [], cluster_mode: "single" })

    await getQueryFn(adminNodesQueryOptions)({} as never)

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "GetNodes",
      credentials: creds,
    })
  })
})

describe("adminVMsQueryOptions", () => {
  it("has the correct query key", () => {
    expect(adminVMsQueryOptions.queryKey).toStrictEqual(["admin", "vms"])
  })

  it("throws when not authenticated", async () => {
    mockGetCredentials.mockReturnValue(null)
    await expect(getQueryFn(adminVMsQueryOptions)({} as never)).rejects.toThrow(
      "Not authenticated",
    )
  })

  it("calls signedFetch with GetVMs action", async () => {
    const creds = {
      accessKeyId: "ASIAak",
      secretAccessKey: "sk",
      sessionToken: "token",
      expiration: new Date(Date.now() + 60_000).toISOString(),
    }
    mockGetCredentials.mockReturnValue(creds)
    mockSignedFetch.mockResolvedValue({ vms: [] })

    await getQueryFn(adminVMsQueryOptions)({} as never)

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "GetVMs",
      credentials: creds,
    })
  })
})

describe("adminStorageStatusQueryOptions", () => {
  it("has the correct query key", () => {
    expect(adminStorageStatusQueryOptions.queryKey).toStrictEqual([
      "admin",
      "storageStatus",
    ])
  })

  it("throws when not authenticated", async () => {
    mockGetCredentials.mockReturnValue(null)
    await expect(
      getQueryFn(adminStorageStatusQueryOptions)({} as never),
    ).rejects.toThrow("Not authenticated")
  })

  it("calls signedFetch with GetStorageStatus action", async () => {
    const creds = {
      accessKeyId: "ASIAak",
      secretAccessKey: "sk",
      sessionToken: "token",
      expiration: new Date(Date.now() + 60_000).toISOString(),
    }
    mockGetCredentials.mockReturnValue(creds)
    mockSignedFetch.mockResolvedValue({})

    await getQueryFn(adminStorageStatusQueryOptions)({} as never)

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "GetStorageStatus",
      credentials: creds,
    })
  })
})
