import { describe, expect, it, vi } from "vitest"

vi.mock("@/lib/auth", () => ({
  getCredentials: vi.fn(),
}))

vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: vi.fn(),
}))

import { getCredentials } from "@/lib/auth"
import { signedFetch } from "@/lib/signed-fetch"
import { callQueryFn } from "@/test/query"

import {
  adminNodesQueryOptions,
  adminOchreCatalogQueryOptions,
  adminStorageStatusQueryOptions,
  adminVMsQueryOptions,
} from "./admin"

const mockGetCredentials = vi.mocked(getCredentials)
const mockSignedFetch = vi.mocked(signedFetch)

describe("adminNodesQueryOptions", () => {
  it("has the correct query key", () => {
    expect(adminNodesQueryOptions.queryKey).toStrictEqual(["admin", "nodes"])
  })

  it("throws when not authenticated", async () => {
    mockGetCredentials.mockReturnValue(null)
    await expect(callQueryFn(adminNodesQueryOptions)).rejects.toThrow(
      "Not authenticated",
    )
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

    await callQueryFn(adminNodesQueryOptions)

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
    await expect(callQueryFn(adminVMsQueryOptions)).rejects.toThrow(
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

    await callQueryFn(adminVMsQueryOptions)

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
    await expect(callQueryFn(adminStorageStatusQueryOptions)).rejects.toThrow(
      "Not authenticated",
    )
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

    await callQueryFn(adminStorageStatusQueryOptions)

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "GetStorageStatus",
      credentials: creds,
    })
  })
})

describe("adminOchreCatalogQueryOptions", () => {
  it("has the correct query key", () => {
    expect(adminOchreCatalogQueryOptions.queryKey).toStrictEqual([
      "admin",
      "ochreCatalog",
    ])
  })

  it("throws when not authenticated", async () => {
    mockGetCredentials.mockReturnValue(null)
    await expect(callQueryFn(adminOchreCatalogQueryOptions)).rejects.toThrow(
      "Not authenticated",
    )
  })

  it("calls signedFetch with ListOchreCatalog action", async () => {
    const creds = {
      accessKeyId: "ASIAak",
      secretAccessKey: "sk",
      sessionToken: "token",
      expiration: new Date(Date.now() + 60_000).toISOString(),
    }
    mockGetCredentials.mockReturnValue(creds)
    mockSignedFetch.mockResolvedValue({ entries: [] })

    await callQueryFn(adminOchreCatalogQueryOptions)

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "ListOchreCatalog",
      credentials: creds,
    })
  })
})
