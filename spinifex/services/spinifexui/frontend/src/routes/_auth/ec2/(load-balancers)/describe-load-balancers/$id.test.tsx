import { screen } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { routerState, sdk } = vi.hoisted(() => {
  interface Command {
    readonly constructor: { name: string }
    readonly input: unknown
  }
  const handlers = new Map<string, (input: unknown) => unknown>()
  const send = vi.fn(async (command: Command): Promise<unknown> => {
    const handler = handlers.get(command.constructor.name)
    if (!handler) {
      throw new Error(
        `No handler registered for SDK command ${command.constructor.name}`,
      )
    }
    return handler(command.input)
  })
  return {
    routerState: { navigate: vi.fn() },
    sdk: {
      send,
      setHandler: (name: string, handler: (input: unknown) => unknown) => {
        handlers.set(name, handler)
      },
      reset: () => {
        handlers.clear()
        send.mockClear()
      },
    },
  }
})

vi.mock("@/lib/awsClient", () => ({
  getElbv2Client: () => ({ send: sdk.send }),
  getEc2Client: () => ({ send: sdk.send }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    createFileRoute: () => (options: Record<string, unknown>) => options,
    useNavigate: () => routerState.navigate,
    Link: ({
      children,
      to,
      className,
    }: {
      children: React.ReactNode
      to?: string
      className?: string
    }) => (
      <a className={className} href={to}>
        {children}
      </a>
    ),
  }
})

import { LoadBalancerDetailPage } from "../-components/load-balancer-detail-page"

const LB_ARN = "arn:lb:1"

describe("load-balancer detail route", () => {
  beforeEach(() => sdk.reset())
  afterEach(() => vi.clearAllMocks())

  function seed() {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers", LB_ARN], {
      LoadBalancers: [
        {
          LoadBalancerArn: LB_ARN,
          LoadBalancerName: "my-alb",
          DNSName: "my-alb.example",
          Type: "application",
          Scheme: "internet-facing",
          IpAddressType: "ipv4",
          VpcId: "vpc-aaa",
          State: { Code: "active" },
          AvailabilityZones: [{ ZoneName: "az-1", SubnetId: "subnet-a" }],
          SecurityGroups: ["sg-1"],
        },
      ],
    })
    qc.setQueryData(["elbv2", "loadBalancers", LB_ARN, "attributes"], {
      Attributes: [{ Key: "deletion_protection.enabled", Value: "false" }],
    })
    qc.setQueryData(["elbv2", "tags", LB_ARN], {
      TagDescriptions: [
        {
          ResourceArn: LB_ARN,
          Tags: [{ Key: "env", Value: "prod" }],
        },
      ],
    })
    qc.setQueryData(["elbv2", "listeners", LB_ARN], { Listeners: [] })
    qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: [] })
    qc.setQueryData(["ec2", "subnets"], {
      Subnets: [{ SubnetId: "subnet-a", CidrBlock: "10.0.1.0/24", Tags: [] }],
    })
    qc.setQueryData(["ec2", "securityGroups"], {
      SecurityGroups: [
        { GroupId: "sg-1", GroupName: "default", VpcId: "vpc-aaa" },
      ],
    })
    return qc
  }

  it("renders overview fields for the LB", () => {
    renderWithClient(<LoadBalancerDetailPage arn={LB_ARN} />, seed())

    expect(screen.getByText("my-alb")).toBeInTheDocument()
    expect(screen.getByText("my-alb.example")).toBeInTheDocument()
    expect(screen.getByText("application")).toBeInTheDocument()
    expect(screen.getByText("internet-facing")).toBeInTheDocument()
    expect(screen.getByText("sg-1")).toBeInTheDocument()
    expect(
      screen.getByRole("tab", { name: "Security groups" }),
    ).toBeInTheDocument()
  })

  it("hides the Security groups tab for a network load balancer", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers", LB_ARN], {
      LoadBalancers: [
        {
          LoadBalancerArn: LB_ARN,
          LoadBalancerName: "my-nlb",
          DNSName: "my-nlb.example",
          Type: "network",
          Scheme: "internet-facing",
          IpAddressType: "ipv4",
          VpcId: "vpc-aaa",
          State: { Code: "active" },
          AvailabilityZones: [{ ZoneName: "az-1", SubnetId: "subnet-a" }],
        },
      ],
    })
    qc.setQueryData(["elbv2", "loadBalancers", LB_ARN, "attributes"], {
      Attributes: [],
    })
    qc.setQueryData(["elbv2", "tags", LB_ARN], { TagDescriptions: [] })
    qc.setQueryData(["elbv2", "listeners", LB_ARN], { Listeners: [] })
    qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: [] })
    qc.setQueryData(["ec2", "subnets"], {
      Subnets: [{ SubnetId: "subnet-a", CidrBlock: "10.0.1.0/24", Tags: [] }],
    })
    qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })

    renderWithClient(<LoadBalancerDetailPage arn={LB_ARN} />, qc)

    expect(screen.getByText("my-nlb")).toBeInTheDocument()
    expect(screen.queryByRole("tab", { name: "Security groups" })).toBeNull()
    expect(screen.getByRole("tab", { name: "Listeners" })).toBeInTheDocument()
  })

  it("renders a not-found state when the LB ARN is unknown", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers", LB_ARN], { LoadBalancers: [] })
    qc.setQueryData(["elbv2", "loadBalancers", LB_ARN, "attributes"], {
      Attributes: [],
    })
    qc.setQueryData(["elbv2", "tags", LB_ARN], { TagDescriptions: [] })
    qc.setQueryData(["ec2", "subnets"], { Subnets: [] })
    qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })

    renderWithClient(<LoadBalancerDetailPage arn={LB_ARN} />, qc)

    expect(screen.getByText("Load balancer not found.")).toBeInTheDocument()
  })
})
