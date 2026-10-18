import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getEksClient: () => ({ send: mockSend }),
}))

import { callQueryFn } from "@/test/query"

import {
  eksAccessEntryQueryOptions,
  eksAddonQueryOptions,
  eksAddonsQueryOptions,
  eksClusterQueryOptions,
  eksClustersQueryOptions,
  eksNodegroupQueryOptions,
} from "./eks"

describe("queryFn", () => {
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
