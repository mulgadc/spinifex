import { beforeEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getEksClient: () => ({ send: mockSend }),
}))

import { callQueryFn } from "@/test/query"

import {
  eksAccessEntriesQueryOptions,
  eksAccessEntryQueryOptions,
  eksAccessPoliciesQueryOptions,
  eksAddonQueryOptions,
  eksAddonsQueryOptions,
  eksAddonVersionsQueryOptions,
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

  it("addon versions catalog key", () => {
    expect(eksAddonVersionsQueryOptions.queryKey).toStrictEqual([
      "eks",
      "addon-versions",
    ])
  })

  it("addons key includes cluster", () => {
    expect(eksAddonsQueryOptions("c1").queryKey).toStrictEqual([
      "eks",
      "clusters",
      "c1",
      "addons",
    ])
  })

  it("addon key includes cluster and addon", () => {
    expect(eksAddonQueryOptions("c1", "coredns").queryKey).toStrictEqual([
      "eks",
      "clusters",
      "c1",
      "addons",
      "coredns",
    ])
  })
})

describe("queryFn", () => {
  beforeEach(() => {
    mockSend.mockClear()
  })

  it("clusters sends ListClustersCommand", async () => {
    await callQueryFn(eksClustersQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("cluster sends DescribeClusterCommand with name", async () => {
    await callQueryFn(eksClusterQueryOptions("c1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ name: "c1" })
  })

  it("nodegroup sends DescribeNodegroupCommand", async () => {
    await callQueryFn(eksNodegroupQueryOptions("c1", "ng1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      clusterName: "c1",
      nodegroupName: "ng1",
    })
  })

  it("access entry sends DescribeAccessEntryCommand", async () => {
    await callQueryFn(eksAccessEntryQueryOptions("c1", "arn:p"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      clusterName: "c1",
      principalArn: "arn:p",
    })
  })

  it("addons sends ListAddonsCommand with cluster", async () => {
    await callQueryFn(eksAddonsQueryOptions("c1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      clusterName: "c1",
    })
  })

  it("addon sends DescribeAddonCommand", async () => {
    await callQueryFn(eksAddonQueryOptions("c1", "coredns"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      clusterName: "c1",
      addonName: "coredns",
    })
  })
})
