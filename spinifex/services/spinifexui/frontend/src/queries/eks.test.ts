import { beforeEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getEksClient: () => ({ send: mockSend }),
}))

import {
  eksAccessEntriesQueryOptions,
  eksAccessEntryQueryOptions,
  eksAccessPoliciesQueryOptions,
  eksAssociatedAccessPoliciesQueryOptions,
  eksClusterQueryOptions,
  eksClustersQueryOptions,
  eksNodegroupQueryOptions,
  eksNodegroupsQueryOptions,
} from "./eks"

describe("query keys", () => {
  it("clusters list key", () => {
    expect(eksClustersQueryOptions.queryKey).toStrictEqual(["eks", "clusters"])
  })

  it("cluster key includes name", () => {
    expect(eksClusterQueryOptions("c1").queryKey).toStrictEqual([
      "eks",
      "clusters",
      "c1",
    ])
  })

  it("nodegroups key includes cluster", () => {
    expect(eksNodegroupsQueryOptions("c1").queryKey).toStrictEqual([
      "eks",
      "clusters",
      "c1",
      "nodegroups",
    ])
  })

  it("nodegroup key includes cluster and nodegroup", () => {
    expect(eksNodegroupQueryOptions("c1", "ng1").queryKey).toStrictEqual([
      "eks",
      "clusters",
      "c1",
      "nodegroups",
      "ng1",
    ])
  })

  it("access entries key includes cluster", () => {
    expect(eksAccessEntriesQueryOptions("c1").queryKey).toStrictEqual([
      "eks",
      "clusters",
      "c1",
      "access-entries",
    ])
  })

  it("associated access policies key includes principal", () => {
    expect(
      eksAssociatedAccessPoliciesQueryOptions("c1", "arn:p").queryKey,
    ).toStrictEqual([
      "eks",
      "clusters",
      "c1",
      "access-entries",
      "arn:p",
      "policies",
    ])
  })

  it("access policies catalog key", () => {
    expect(eksAccessPoliciesQueryOptions.queryKey).toStrictEqual([
      "eks",
      "access-policies",
    ])
  })
})

describe("queryFn", () => {
  beforeEach(() => {
    mockSend.mockClear()
  })

  it("clusters sends ListClustersCommand", async () => {
    const queryFn = eksClustersQueryOptions.queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("cluster sends DescribeClusterCommand with name", async () => {
    const queryFn = eksClusterQueryOptions("c1").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ name: "c1" })
  })

  it("nodegroup sends DescribeNodegroupCommand", async () => {
    const queryFn = eksNodegroupQueryOptions("c1", "ng1").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      clusterName: "c1",
      nodegroupName: "ng1",
    })
  })

  it("access entry sends DescribeAccessEntryCommand", async () => {
    const queryFn = eksAccessEntryQueryOptions("c1", "arn:p").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      clusterName: "c1",
      principalArn: "arn:p",
    })
  })
})
