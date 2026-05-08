import { afterEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getElbv2Client: () => ({ send: mockSend }),
}))

import {
  elbv2ListenersQueryOptions,
  elbv2LoadBalancerAttributesQueryOptions,
  elbv2LoadBalancerQueryOptions,
  elbv2LoadBalancersQueryOptions,
  elbv2TagsQueryOptions,
  elbv2TargetGroupAttributesQueryOptions,
  elbv2TargetGroupQueryOptions,
  elbv2TargetGroupsQueryOptions,
  elbv2TargetHealthQueryOptions,
} from "./elbv2"

afterEach(() => {
  mockSend.mockReset()
})

describe("elbv2 query keys", () => {
  it("elbv2LoadBalancersQueryOptions has correct key", () => {
    expect(elbv2LoadBalancersQueryOptions.queryKey).toEqual([
      "elbv2",
      "loadBalancers",
    ])
  })

  it("elbv2LoadBalancerQueryOptions includes arn", () => {
    expect(elbv2LoadBalancerQueryOptions("arn:lb").queryKey).toEqual([
      "elbv2",
      "loadBalancers",
      "arn:lb",
    ])
  })

  it("elbv2LoadBalancerAttributesQueryOptions includes arn + attributes", () => {
    expect(elbv2LoadBalancerAttributesQueryOptions("arn:lb").queryKey).toEqual([
      "elbv2",
      "loadBalancers",
      "arn:lb",
      "attributes",
    ])
  })

  it("elbv2TargetGroupsQueryOptions has correct key", () => {
    expect(elbv2TargetGroupsQueryOptions.queryKey).toEqual([
      "elbv2",
      "targetGroups",
    ])
  })

  it("elbv2TargetGroupQueryOptions includes arn", () => {
    expect(elbv2TargetGroupQueryOptions("arn:tg").queryKey).toEqual([
      "elbv2",
      "targetGroups",
      "arn:tg",
    ])
  })

  it("elbv2TargetGroupAttributesQueryOptions includes arn + attributes", () => {
    expect(elbv2TargetGroupAttributesQueryOptions("arn:tg").queryKey).toEqual([
      "elbv2",
      "targetGroups",
      "arn:tg",
      "attributes",
    ])
  })

  it("elbv2ListenersQueryOptions includes lb arn", () => {
    expect(elbv2ListenersQueryOptions("arn:lb").queryKey).toEqual([
      "elbv2",
      "listeners",
      "arn:lb",
    ])
  })

  it("elbv2TargetHealthQueryOptions includes tg arn + health", () => {
    expect(elbv2TargetHealthQueryOptions("arn:tg").queryKey).toEqual([
      "elbv2",
      "targetGroups",
      "arn:tg",
      "health",
    ])
  })

  it("elbv2TagsQueryOptions spreads resource arns into key", () => {
    expect(elbv2TagsQueryOptions(["arn:lb", "arn:tg"]).queryKey).toEqual([
      "elbv2",
      "tags",
      "arn:lb",
      "arn:tg",
    ])
  })
})

type QueryFnWithSignal = (ctx: { signal: AbortSignal }) => Promise<unknown>

async function callQueryFn(queryFn: unknown): Promise<unknown> {
  return await (queryFn as QueryFnWithSignal)({
    signal: new AbortController().signal,
  })
}

describe("elbv2 implemented queries send the right command", () => {
  it("loadBalancers list sends DescribeLoadBalancersCommand", async () => {
    mockSend.mockResolvedValueOnce({ LoadBalancers: [] })
    await callQueryFn(elbv2LoadBalancersQueryOptions.queryFn)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({})
  })

  it("loadBalancer detail filters by ARN", async () => {
    mockSend.mockResolvedValueOnce({ LoadBalancers: [] })
    await callQueryFn(elbv2LoadBalancerQueryOptions("arn:lb").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({
      LoadBalancerArns: ["arn:lb"],
    })
  })

  it("loadBalancer attributes sends arn", async () => {
    mockSend.mockResolvedValueOnce({ Attributes: [] })
    await callQueryFn(elbv2LoadBalancerAttributesQueryOptions("arn:lb").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({
      LoadBalancerArn: "arn:lb",
    })
  })

  it("listeners filters by load balancer arn", async () => {
    mockSend.mockResolvedValueOnce({ Listeners: [] })
    await callQueryFn(elbv2ListenersQueryOptions("arn:lb").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({
      LoadBalancerArn: "arn:lb",
    })
  })

  it("tags sends resource arns", async () => {
    mockSend.mockResolvedValueOnce({ TagDescriptions: [] })
    await callQueryFn(elbv2TagsQueryOptions(["arn:lb"]).queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({
      ResourceArns: ["arn:lb"],
    })
  })

  it("targetGroups list sends DescribeTargetGroupsCommand", async () => {
    mockSend.mockResolvedValueOnce({ TargetGroups: [] })
    await callQueryFn(elbv2TargetGroupsQueryOptions.queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({})
  })

  it("targetGroup detail filters by ARN", async () => {
    mockSend.mockResolvedValueOnce({ TargetGroups: [] })
    await callQueryFn(elbv2TargetGroupQueryOptions("arn:tg").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({
      TargetGroupArns: ["arn:tg"],
    })
  })

  it("targetGroup attributes sends arn", async () => {
    mockSend.mockResolvedValueOnce({ Attributes: [] })
    await callQueryFn(elbv2TargetGroupAttributesQueryOptions("arn:tg").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({
      TargetGroupArn: "arn:tg",
    })
  })

  it("targetHealth sends target group arn", async () => {
    mockSend.mockResolvedValueOnce({ TargetHealthDescriptions: [] })
    await callQueryFn(elbv2TargetHealthQueryOptions("arn:tg").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toEqual({
      TargetGroupArn: "arn:tg",
    })
  })
})

describe("elbv2 load balancer list poll cadence", () => {
  it("polls every 5s while any lb is provisioning", () => {
    const refetch = elbv2LoadBalancersQueryOptions.refetchInterval
    if (typeof refetch !== "function") {
      throw new TypeError("expected refetchInterval to be a function")
    }
    const result = refetch({
      state: {
        data: { LoadBalancers: [{ State: { Code: "provisioning" } }] },
      },
    } as never)
    expect(result).toBe(5000)
  })

  it("does not poll once all lbs are active", () => {
    const refetch = elbv2LoadBalancersQueryOptions.refetchInterval
    if (typeof refetch !== "function") {
      throw new TypeError("expected refetchInterval to be a function")
    }
    const result = refetch({
      state: {
        data: { LoadBalancers: [{ State: { Code: "active" } }] },
      },
    } as never)
    expect(result).toBe(false)
  })
})
