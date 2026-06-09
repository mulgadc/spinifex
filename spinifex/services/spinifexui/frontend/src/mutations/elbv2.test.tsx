import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getElbv2Client: () => ({ send: mockSend }),
}))

import type { CreateTargetGroupFormData } from "@/types/elbv2"

import {
  useCreateListener,
  useCreateLoadBalancerWizard,
  useCreateTargetGroup,
  useDeleteListener,
  useDeleteLoadBalancer,
  useDeleteTargetGroup,
  useDeregisterTargets,
  useModifyLoadBalancerAttributes,
  useModifyTargetGroupAttributes,
  useRegisterTargets,
} from "./elbv2"

const TG_ARN = "arn:aws:elasticloadbalancing:tg/app/foo/abc"

let queryClient: QueryClient

function wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function createQueryClient() {
  queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  return queryClient
}

describe("useModifyLoadBalancerAttributes", () => {
  it("sends ModifyLoadBalancerAttributesCommand with arn + changed attributes", async () => {
    createQueryClient()
    const { result } = renderHook(() => useModifyLoadBalancerAttributes(), {
      wrapper,
    })

    result.current.mutate({
      loadBalancerArn: "arn:lb:1",
      attributes: [
        { key: "deletion_protection.enabled", value: "true" },
        { key: "idle_timeout.timeout_seconds", value: "120" },
      ],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LoadBalancerArn: "arn:lb:1",
      Attributes: [
        { Key: "deletion_protection.enabled", Value: "true" },
        { Key: "idle_timeout.timeout_seconds", Value: "120" },
      ],
    })
  })
})

describe("useModifyTargetGroupAttributes", () => {
  it("sends ModifyTargetGroupAttributesCommand with arn + changed attributes", async () => {
    createQueryClient()
    const { result } = renderHook(() => useModifyTargetGroupAttributes(), {
      wrapper,
    })

    result.current.mutate({
      targetGroupArn: TG_ARN,
      attributes: [{ key: "stickiness.enabled", value: "true" }],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      TargetGroupArn: TG_ARN,
      Attributes: [{ Key: "stickiness.enabled", Value: "true" }],
    })
  })
})

describe("useDeleteLoadBalancer", () => {
  it("sends DeleteLoadBalancerCommand with load balancer ARN", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteLoadBalancer(), { wrapper })

    result.current.mutate("arn:aws:elasticloadbalancing:lb/app/foo/abc")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LoadBalancerArn: "arn:aws:elasticloadbalancing:lb/app/foo/abc",
    })
  })
})

describe("useDeleteTargetGroup", () => {
  it("sends DeleteTargetGroupCommand with target group ARN", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteTargetGroup(), { wrapper })

    result.current.mutate("arn:aws:elasticloadbalancing:tg/app/foo/abc")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      TargetGroupArn: "arn:aws:elasticloadbalancing:tg/app/foo/abc",
    })
  })
})

describe("useCreateTargetGroup", () => {
  const baseParams: CreateTargetGroupFormData = {
    name: "my-tg",
    protocol: "HTTP",
    port: 80,
    vpcId: "vpc-123",
    healthCheck: {
      protocol: "HTTP",
      path: "/health",
      port: "traffic-port",
      intervalSeconds: 30,
      timeoutSeconds: 5,
      healthyThresholdCount: 5,
      unhealthyThresholdCount: 2,
      matcher: "200",
    },
    tags: [],
  }

  it("sends CreateTargetGroupCommand with form data and hardcoded instance target type", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateTargetGroup(), { wrapper })

    result.current.mutate(baseParams)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Name: "my-tg",
      Protocol: "HTTP",
      Port: 80,
      VpcId: "vpc-123",
      TargetType: "instance",
      HealthCheckProtocol: "HTTP",
      HealthCheckPath: "/health",
      HealthCheckPort: "traffic-port",
      HealthCheckIntervalSeconds: 30,
      HealthCheckTimeoutSeconds: 5,
      HealthyThresholdCount: 5,
      UnhealthyThresholdCount: 2,
      Matcher: { HttpCode: "200" },
      Tags: undefined,
    })
  })

  it("passes non-empty tags through and skips empty keys", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateTargetGroup(), { wrapper })

    result.current.mutate({
      ...baseParams,
      tags: [
        { key: "env", value: "prod" },
        { key: "", value: "ignored" },
      ],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Tags).toStrictEqual([
      { Key: "env", Value: "prod" },
    ])
  })
})

describe("useCreateListener", () => {
  it("sends CreateListenerCommand with forward default action", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateListener(), { wrapper })

    result.current.mutate({
      loadBalancerArn: "arn:lb:1",
      protocol: "HTTP",
      port: 80,
      defaultTargetGroupArn: "arn:tg:1",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LoadBalancerArn: "arn:lb:1",
      Protocol: "HTTP",
      Port: 80,
      DefaultActions: [{ Type: "forward", TargetGroupArn: "arn:tg:1" }],
    })
  })
})

describe("useRegisterTargets", () => {
  it("sends RegisterTargetsCommand with id-only targets when port omitted", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRegisterTargets(), { wrapper })

    result.current.mutate({
      targetGroupArn: TG_ARN,
      targets: [{ id: "i-aaa" }, { id: "i-bbb" }],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      TargetGroupArn: TG_ARN,
      Targets: [
        { Id: "i-aaa", Port: undefined },
        { Id: "i-bbb", Port: undefined },
      ],
    })
  })

  it("passes per-target port override through", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRegisterTargets(), { wrapper })

    result.current.mutate({
      targetGroupArn: TG_ARN,
      targets: [{ id: "i-aaa", port: 8080 }],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Targets).toStrictEqual([
      { Id: "i-aaa", Port: 8080 },
    ])
  })
})

describe("useDeregisterTargets", () => {
  it("sends DeregisterTargetsCommand with target id and port", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeregisterTargets(), { wrapper })

    result.current.mutate({
      targetGroupArn: TG_ARN,
      targets: [{ id: "i-aaa", port: 80 }],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      TargetGroupArn: TG_ARN,
      Targets: [{ Id: "i-aaa", Port: 80 }],
    })
  })
})

describe("useDeleteListener", () => {
  it("sends DeleteListenerCommand with listener ARN", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteListener(), { wrapper })

    result.current.mutate("arn:listener:1")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      ListenerArn: "arn:listener:1",
    })
  })
})

describe("useCreateLoadBalancerWizard", () => {
  const tgParams: CreateTargetGroupFormData = {
    name: "my-tg",
    protocol: "HTTP",
    port: 80,
    vpcId: "vpc-123",
    healthCheck: {
      protocol: "HTTP",
      path: "/",
      port: "traffic-port",
      intervalSeconds: 30,
      timeoutSeconds: 5,
      healthyThresholdCount: 5,
      unhealthyThresholdCount: 2,
      matcher: "200",
    },
    tags: [],
  }

  const lbBase = {
    name: "my-alb",
    type: "application" as const,
    scheme: "internet-facing" as const,
    vpcId: "vpc-123",
    subnetIds: ["subnet-a", "subnet-b"],
    securityGroupIds: ["sg-1"],
    tags: [],
  }

  it("creates TG → LB → Listener on happy path with new target group", async () => {
    createQueryClient()
    mockSend.mockReset()
    mockSend
      .mockResolvedValueOnce({
        TargetGroups: [{ TargetGroupArn: "arn:tg:new" }],
      })
      .mockResolvedValueOnce({
        LoadBalancers: [{ LoadBalancerArn: "arn:lb:new" }],
      })
      .mockResolvedValueOnce({})
    const { result } = renderHook(() => useCreateLoadBalancerWizard(), {
      wrapper,
    })

    result.current.mutate({
      lb: lbBase,
      listener: {
        protocol: "HTTP",
        port: 80,
        targetGroupMode: "new",
        newTargetGroup: tgParams,
      },
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(result.current.data?.error).toBeUndefined()
    expect(result.current.data?.loadBalancerArn).toBe("arn:lb:new")
    expect(result.current.data?.created).toHaveLength(3)
    expect(mockSend).toHaveBeenCalledTimes(3)
  })

  it("skips TG creation when mode=existing", async () => {
    createQueryClient()
    mockSend.mockReset()
    mockSend
      .mockResolvedValueOnce({
        LoadBalancers: [{ LoadBalancerArn: "arn:lb:new" }],
      })
      .mockResolvedValueOnce({})
    const { result } = renderHook(() => useCreateLoadBalancerWizard(), {
      wrapper,
    })

    result.current.mutate({
      lb: lbBase,
      listener: {
        protocol: "HTTP",
        port: 80,
        targetGroupMode: "existing",
        existingTargetGroupArn: "arn:tg:existing",
      },
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend).toHaveBeenCalledTimes(2)
    expect(result.current.data?.created).toHaveLength(2)
  })

  it("surfaces partial creation when LB step fails", async () => {
    createQueryClient()
    mockSend.mockReset()
    mockSend
      .mockResolvedValueOnce({
        TargetGroups: [{ TargetGroupArn: "arn:tg:new" }],
      })
      .mockRejectedValueOnce(new Error("lb boom"))
    const { result } = renderHook(() => useCreateLoadBalancerWizard(), {
      wrapper,
    })

    result.current.mutate({
      lb: lbBase,
      listener: {
        protocol: "HTTP",
        port: 80,
        targetGroupMode: "new",
        newTargetGroup: tgParams,
      },
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(result.current.data?.error?.message).toBe("lb boom")
    expect(result.current.data?.failedStep).toBe("creating load balancer")
    expect(result.current.data?.created).toStrictEqual([
      { type: "Target Group", id: "arn:tg:new" },
    ])
    expect(result.current.data?.loadBalancerArn).toBeUndefined()
  })
})
