import { describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { callQueryFn, callRefetchInterval } from "@/test/query"

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

describe("elbv2 implemented queries send the right command", () => {
  it("loadBalancers list sends DescribeLoadBalancersCommand", async () => {
    mockSend.mockResolvedValueOnce({ LoadBalancers: [] })
    await callQueryFn(elbv2LoadBalancersQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("loadBalancer detail filters by ARN", async () => {
    mockSend.mockResolvedValueOnce({ LoadBalancers: [] })
    await callQueryFn(elbv2LoadBalancerQueryOptions("arn:lb"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LoadBalancerArns: ["arn:lb"],
    })
  })

  it("loadBalancer attributes sends arn", async () => {
    mockSend.mockResolvedValueOnce({ Attributes: [] })
    await callQueryFn(elbv2LoadBalancerAttributesQueryOptions("arn:lb"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LoadBalancerArn: "arn:lb",
    })
  })

  it("listeners filters by load balancer arn", async () => {
    mockSend.mockResolvedValueOnce({ Listeners: [] })
    await callQueryFn(elbv2ListenersQueryOptions("arn:lb"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LoadBalancerArn: "arn:lb",
    })
  })

  it("tags sends resource arns", async () => {
    mockSend.mockResolvedValueOnce({ TagDescriptions: [] })
    await callQueryFn(elbv2TagsQueryOptions(["arn:lb"]))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      ResourceArns: ["arn:lb"],
    })
  })

  it("targetGroups list sends DescribeTargetGroupsCommand", async () => {
    mockSend.mockResolvedValueOnce({ TargetGroups: [] })
    await callQueryFn(elbv2TargetGroupsQueryOptions)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("targetGroup detail filters by ARN", async () => {
    mockSend.mockResolvedValueOnce({ TargetGroups: [] })
    await callQueryFn(elbv2TargetGroupQueryOptions("arn:tg"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      TargetGroupArns: ["arn:tg"],
    })
  })

  it("targetGroup attributes sends arn", async () => {
    mockSend.mockResolvedValueOnce({ Attributes: [] })
    await callQueryFn(elbv2TargetGroupAttributesQueryOptions("arn:tg"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      TargetGroupArn: "arn:tg",
    })
  })

  it("targetHealth sends target group arn", async () => {
    mockSend.mockResolvedValueOnce({ TargetHealthDescriptions: [] })
    await callQueryFn(elbv2TargetHealthQueryOptions("arn:tg"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      TargetGroupArn: "arn:tg",
    })
  })
})

describe("elbv2 load balancer list poll cadence", () => {
  it("polls every 5s while any lb is provisioning", () => {
    const result = callRefetchInterval(elbv2LoadBalancersQueryOptions, {
      LoadBalancers: [{ State: { Code: "provisioning" } }],
    })

    expect(result).toBe(5000)
  })

  it("does not poll once all lbs are active", () => {
    const result = callRefetchInterval(elbv2LoadBalancersQueryOptions, {
      LoadBalancers: [{ State: { Code: "active" } }],
    })

    expect(result).toBeFalsy()
  })
})
