import { describe, expect, it, vi } from "vitest"

vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: vi.fn(),
  signedAdminFetch: vi.fn(),
}))

import { signedAdminFetch, signedFetch } from "@/lib/signed-fetch"
import { callQueryFn } from "@/test/query"

import {
  adminListAccountsQueryOptions,
  adminModelAccessQueryOptions,
  adminNodesQueryOptions,
  adminOchreCatalogQueryOptions,
  adminStorageStatusQueryOptions,
  adminVMsQueryOptions,
  grantModelAccess,
  instanceTypeCapacityQueryOptions,
  revokeModelAccess,
} from "./admin"

const mockSignedFetch = vi.mocked(signedFetch)
const mockSignedAdminFetch = vi.mocked(signedAdminFetch)

describe("adminNodesQueryOptions", () => {
  it("calls signedFetch with GetNodes action", async () => {
    mockSignedFetch.mockResolvedValue({ nodes: [], cluster_mode: "single" })

    await callQueryFn(adminNodesQueryOptions)

    expect(mockSignedFetch).toHaveBeenCalledWith({ action: "GetNodes" })
  })
})

describe("instanceTypeCapacityQueryOptions", () => {
  it("calls signedFetch with GetInstanceTypeCapacity action", async () => {
    mockSignedFetch.mockResolvedValue({
      available: { "t3.micro": 7 },
      nodes: 1,
    })

    const data = await callQueryFn(instanceTypeCapacityQueryOptions)

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "GetInstanceTypeCapacity",
    })
    expect(data).toStrictEqual({ available: { "t3.micro": 7 }, nodes: 1 })
  })

  // The action names no resource, so a policy that does not grant it answers
  // AccessDenied. Retrying that would hold the launch panel on a refusal that
  // will not change.
  it("does not retry", () => {
    expect(instanceTypeCapacityQueryOptions).toHaveProperty("retry", false)
  })
})

describe("adminVMsQueryOptions", () => {
  it("calls signedFetch with GetVMs action", async () => {
    mockSignedFetch.mockResolvedValue({ vms: [] })

    await callQueryFn(adminVMsQueryOptions)

    expect(mockSignedFetch).toHaveBeenCalledWith({ action: "GetVMs" })
  })
})

describe("adminStorageStatusQueryOptions", () => {
  it("calls signedFetch with GetStorageStatus action", async () => {
    mockSignedFetch.mockResolvedValue({})

    await callQueryFn(adminStorageStatusQueryOptions)

    expect(mockSignedFetch).toHaveBeenCalledWith({ action: "GetStorageStatus" })
  })
})

describe("adminOchreCatalogQueryOptions", () => {
  it("calls signedFetch with ListOchreCatalog action", async () => {
    mockSignedFetch.mockResolvedValue({ entries: [] })

    await callQueryFn(adminOchreCatalogQueryOptions)

    expect(mockSignedFetch).toHaveBeenCalledWith({ action: "ListOchreCatalog" })
  })
})

describe("adminListAccountsQueryOptions", () => {
  it("calls signedAdminFetch with the ListAccounts method and an empty body", async () => {
    mockSignedAdminFetch.mockResolvedValue({ accounts: [], count: 0 })

    await callQueryFn(adminListAccountsQueryOptions)

    expect(mockSignedAdminFetch).toHaveBeenCalledWith({
      method: "ListAccounts",
      body: {},
    })
  })
})

describe("adminModelAccessQueryOptions", () => {
  it("is disabled when the account id is empty", () => {
    expect(adminModelAccessQueryOptions("").enabled).toBeFalsy()
  })

  it("is enabled when the account id is non-empty", () => {
    expect(adminModelAccessQueryOptions("000000000002").enabled).toBeTruthy()
  })

  it("calls signedFetch with ListModelAccess action and the account id", async () => {
    mockSignedFetch.mockResolvedValue({
      AccountId: "000000000002",
      ModelIds: [],
    })

    await callQueryFn(adminModelAccessQueryOptions("000000000002"))

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "ListModelAccess",
      params: { AccountId: "000000000002" },
    })
  })
})

describe("grantModelAccess", () => {
  it("calls signedFetch with GrantModelAccess action and params", async () => {
    mockSignedFetch.mockResolvedValue({
      AccountId: "000000000002",
      ModelId: "model-a",
    })

    await grantModelAccess({ accountId: "000000000002", modelId: "model-a" })

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "GrantModelAccess",
      params: { AccountId: "000000000002", ModelId: "model-a" },
    })
  })
})

describe("revokeModelAccess", () => {
  it("calls signedFetch with RevokeModelAccess action and params", async () => {
    mockSignedFetch.mockResolvedValue({
      AccountId: "000000000002",
      ModelId: "model-a",
    })

    await revokeModelAccess({ accountId: "000000000002", modelId: "model-a" })

    expect(mockSignedFetch).toHaveBeenCalledWith({
      action: "RevokeModelAccess",
      params: { AccountId: "000000000002", ModelId: "model-a" },
    })
  })
})
