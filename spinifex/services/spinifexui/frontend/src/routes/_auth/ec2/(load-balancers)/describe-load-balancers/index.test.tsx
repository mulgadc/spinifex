import type { Image } from "@aws-sdk/client-ec2"
import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
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
  const send = vi.fn((command: Command): Promise<unknown> => {
    const handler = handlers.get(command.constructor.name)
    if (!handler) {
      return Promise.reject(
        new Error(
          `No handler registered for SDK command ${command.constructor.name}`,
        ),
      )
    }
    return Promise.resolve(handler(command.input))
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

import { DescribeLoadBalancersPage } from "../-components/describe-load-balancers-page"

const LBS: LoadBalancer[] = [
  {
    LoadBalancerArn: "arn:lb:1",
    LoadBalancerName: "lb-one",
    DNSName: "lb-one.example",
    Type: "application",
    Scheme: "internet-facing",
    State: { Code: "active" },
    VpcId: "vpc-aaa",
  },
]

const LB_IMAGE: Image = {
  ImageId: "ami-lb",
  Name: "lb-alpine-3.21.6-x86_64",
  Tags: [{ Key: "spinifex:managed-by", Value: "elbv2" }],
}

describe("describe-load-balancers list route", () => {
  beforeEach(() => sdk.reset())
  afterEach(() => vi.clearAllMocks())

  it("renders load-balancer rows with resolved fields", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers"], { LoadBalancers: LBS })
    qc.setQueryData(["ec2", "images"], { Images: [LB_IMAGE] })

    renderWithClient(<DescribeLoadBalancersPage />, qc)

    expect(screen.getByText("lb-one")).toBeInTheDocument()
    expect(screen.getByText("lb-one.example")).toBeInTheDocument()
    expect(screen.getByText("application")).toBeInTheDocument()
    expect(screen.getByText("internet-facing")).toBeInTheDocument()
    expect(
      screen.queryByText("Load balancer image not imported"),
    ).not.toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Load Balancer" }),
    ).not.toBeDisabled()
  })

  it("shows empty state when no load balancers", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers"], { LoadBalancers: [] })
    qc.setQueryData(["ec2", "images"], { Images: [LB_IMAGE] })

    renderWithClient(<DescribeLoadBalancersPage />, qc)

    expect(screen.getByText("No load balancers found.")).toBeInTheDocument()
  })

  it("shows import banner and disables Create button when LB AMI missing", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers"], { LoadBalancers: [] })
    qc.setQueryData(["ec2", "images"], { Images: [] })

    renderWithClient(<DescribeLoadBalancersPage />, qc)

    expect(
      screen.getByText("Load balancer image not imported"),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/spx admin images import --name lb-alpine/),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Load Balancer" }),
    ).toBeDisabled()
  })

  it("treats non-LB system images as missing", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers"], { LoadBalancers: [] })
    qc.setQueryData(["ec2", "images"], {
      Images: [
        {
          ImageId: "ami-customer",
          Tags: [{ Key: "Name", Value: "ubuntu" }],
        },
      ],
    })

    renderWithClient(<DescribeLoadBalancersPage />, qc)

    expect(
      screen.getByText("Load balancer image not imported"),
    ).toBeInTheDocument()
  })
})
