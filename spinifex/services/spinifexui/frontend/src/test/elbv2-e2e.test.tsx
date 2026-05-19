// Cross-slice ELBv2 flow against a mocked SDK. Exercises the end-to-end path
// a user walks through: create TG → create LB → create listener → register
// targets → observe target health — all via the same mocked dispatcher.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook } from "@testing-library/react"
import type { ReactNode } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

const { sdk } = vi.hoisted(() => {
  interface StoredLb {
    arn: string
    name: string
    vpcId: string
    subnets: string[]
  }
  interface StoredTg {
    arn: string
    name: string
    vpcId: string
  }
  interface StoredListener {
    arn: string
    lbArn: string
    port: number
    protocol: string
    defaultTg: string
  }
  interface TargetRecord {
    id: string
    port?: number
    state: string
  }
  interface Command {
    readonly constructor: { name: string }
    readonly input: unknown
  }

  const state = {
    lbs: [] as StoredLb[],
    tgs: [] as StoredTg[],
    listeners: [] as StoredListener[],
    targets: new Map<string, TargetRecord[]>(),
    seq: 0,
  }

  const nextSeq = () => {
    state.seq += 1
    return state.seq
  }

  const handlers = new Map<string, (input: unknown) => unknown>([
    [
      "CreateTargetGroupCommand",
      (input) => {
        const i = input as { Name: string; VpcId: string }
        const tg = {
          arn: `arn:tg:${nextSeq()}`,
          name: i.Name,
          vpcId: i.VpcId,
        }
        state.tgs.push(tg)
        state.targets.set(tg.arn, [])
        return { TargetGroups: [{ TargetGroupArn: tg.arn }] }
      },
    ],
    [
      "CreateLoadBalancerCommand",
      (input) => {
        const i = input as { Name: string; Subnets: string[] }
        const lb = {
          arn: `arn:lb:${nextSeq()}`,
          name: i.Name,
          vpcId: "vpc-aaa",
          subnets: i.Subnets,
        }
        state.lbs.push(lb)
        return { LoadBalancers: [{ LoadBalancerArn: lb.arn }] }
      },
    ],
    [
      "CreateListenerCommand",
      (input) => {
        const i = input as {
          LoadBalancerArn: string
          Port: number
          Protocol: string
          DefaultActions: { TargetGroupArn: string }[]
        }
        const action = i.DefaultActions[0]
        if (!action) {
          throw new Error("CreateListenerCommand requires DefaultActions[0]")
        }
        const listener = {
          arn: `arn:listener:${nextSeq()}`,
          lbArn: i.LoadBalancerArn,
          port: i.Port,
          protocol: i.Protocol,
          defaultTg: action.TargetGroupArn,
        }
        state.listeners.push(listener)
        return { Listeners: [{ ListenerArn: listener.arn }] }
      },
    ],
    [
      "RegisterTargetsCommand",
      (input) => {
        const i = input as {
          TargetGroupArn: string
          Targets: { Id: string; Port?: number }[]
        }
        const existing = state.targets.get(i.TargetGroupArn) ?? []
        const next = [
          ...existing,
          ...i.Targets.map((t) => ({
            id: t.Id,
            port: t.Port,
            state: "healthy",
          })),
        ]
        state.targets.set(i.TargetGroupArn, next)
        return {}
      },
    ],
    [
      "DescribeTargetHealthCommand",
      (input) => {
        const i = input as { TargetGroupArn: string }
        const targets = state.targets.get(i.TargetGroupArn) ?? []
        return {
          TargetHealthDescriptions: targets.map((t) => ({
            Target: { Id: t.id, Port: t.port },
            TargetHealth: { State: t.state },
          })),
        }
      },
    ],
    [
      "DescribeLoadBalancersCommand",
      () => ({
        LoadBalancers: state.lbs.map((lb) => ({
          LoadBalancerArn: lb.arn,
          LoadBalancerName: lb.name,
          VpcId: lb.vpcId,
          State: { Code: "active" },
        })),
      }),
    ],
    [
      "DescribeTargetGroupsCommand",
      () => ({
        TargetGroups: state.tgs.map((tg) => ({
          TargetGroupArn: tg.arn,
          TargetGroupName: tg.name,
          VpcId: tg.vpcId,
          Protocol: "HTTP",
          Port: 80,
          TargetType: "instance",
        })),
      }),
    ],
    [
      "DescribeListenersCommand",
      (input) => {
        const i = input as { LoadBalancerArn: string }
        return {
          Listeners: state.listeners
            .filter((l) => l.lbArn === i.LoadBalancerArn)
            .map((l) => ({
              ListenerArn: l.arn,
              Port: l.port,
              Protocol: l.protocol,
              DefaultActions: [
                { Type: "forward", TargetGroupArn: l.defaultTg },
              ],
            })),
        }
      },
    ],
  ])

  const send = vi.fn(async (command: Command): Promise<unknown> => {
    const handler = handlers.get(command.constructor.name)
    if (!handler) {
      throw new Error(
        `No E2E handler for SDK command ${command.constructor.name}`,
      )
    }
    return handler(command.input)
  })

  return {
    sdk: {
      send,
      reset: () => {
        state.lbs = []
        state.tgs = []
        state.listeners = []
        state.targets = new Map()
        state.seq = 0
        send.mockClear()
      },
    },
  }
})

vi.mock("@/lib/awsClient", () => ({
  getElbv2Client: () => ({ send: sdk.send }),
  getEc2Client: () => ({ send: sdk.send }),
}))

import {
  useCreateListener,
  useCreateLoadBalancer,
  useCreateTargetGroup,
  useRegisterTargets,
} from "@/mutations/elbv2"
import {
  elbv2ListenersQueryOptions,
  elbv2LoadBalancersQueryOptions,
  elbv2TargetGroupsQueryOptions,
  elbv2TargetHealthQueryOptions,
} from "@/queries/elbv2"

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
}

function mustString(value: string | undefined, label: string): string {
  if (!value) {
    throw new Error(`expected ${label} to be defined`)
  }
  return value
}

function Harness() {
  return {
    createTg: useCreateTargetGroup(),
    createLb: useCreateLoadBalancer(),
    createListener: useCreateListener(),
    registerTargets: useRegisterTargets(),
  }
}

describe("ELBv2 cross-slice flow (mocked SDK)", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("creates TG → LB → listener → registers targets → observes healthy state", async () => {
    const qc = createQueryClient()

    const { result } = renderHook(Harness, {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={qc}>{children}</QueryClientProvider>
      ),
    })

    // Step 1: create TG
    const tgResult = await result.current.createTg.mutateAsync({
      name: "my-tg",
      protocol: "HTTP",
      port: 80,
      vpcId: "vpc-aaa",
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
    })
    const tgArn = mustString(
      tgResult.TargetGroups?.[0]?.TargetGroupArn,
      "tgArn",
    )

    // Step 2: create LB
    const lbResult = await result.current.createLb.mutateAsync({
      name: "my-alb",
      scheme: "internet-facing",
      subnetIds: ["subnet-a", "subnet-b"],
      securityGroupIds: ["sg-1"],
      tags: [],
    })
    const lbArn = mustString(
      lbResult.LoadBalancers?.[0]?.LoadBalancerArn,
      "lbArn",
    )

    // Step 3: create listener
    await result.current.createListener.mutateAsync({
      loadBalancerArn: lbArn,
      protocol: "HTTP",
      port: 80,
      defaultTargetGroupArn: tgArn,
    })

    // Step 4: register two targets
    await result.current.registerTargets.mutateAsync({
      targetGroupArn: tgArn,
      targets: [{ id: "i-aaa" }, { id: "i-bbb" }],
    })

    // Step 5: describe queries should reflect all the writes
    const lbs = await qc.fetchQuery(elbv2LoadBalancersQueryOptions)
    expect(lbs.LoadBalancers).toHaveLength(1)
    expect(lbs.LoadBalancers?.[0]?.LoadBalancerArn).toBe(lbArn)

    const tgs = await qc.fetchQuery(elbv2TargetGroupsQueryOptions)
    expect(tgs.TargetGroups).toHaveLength(1)

    const listeners = await qc.fetchQuery(elbv2ListenersQueryOptions(lbArn))
    expect(listeners.Listeners).toHaveLength(1)
    expect(listeners.Listeners?.[0]?.DefaultActions?.[0]?.TargetGroupArn).toBe(
      tgArn,
    )

    const health = await qc.fetchQuery(elbv2TargetHealthQueryOptions(tgArn))
    expect(health.TargetHealthDescriptions).toHaveLength(2)
    expect(health.TargetHealthDescriptions?.[0]?.TargetHealth?.State).toBe(
      "healthy",
    )
  })
})
