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
  type SdkHandler = (input: never) => unknown
  const handlers = new Map<string, SdkHandler>()
  const send = vi.fn(async (command: Command): Promise<unknown> => {
    const handler = handlers.get(command.constructor.name)
    if (!handler) {
      throw new Error(
        `No handler registered for SDK command ${command.constructor.name}`,
      )
    }
    return handler(command.input as never)
  })
  return {
    routerState: { navigate: vi.fn() },
    sdk: {
      send,
      // Handlers are keyed by command name, so each one declares the input type
      // of its own command; the shared registry cannot name them all.
      setHandler: (name: string, handler: SdkHandler) => {
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
    createFileRoute:
      () =>
      <TOptions,>(options: TOptions): TOptions =>
        options,
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

describe("describe-load-balancers list route", () => {
  beforeEach(() => sdk.reset())
  afterEach(() => vi.clearAllMocks())

  it("renders load-balancer rows with resolved fields", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers"], { LoadBalancers: LBS })

    renderWithClient(<DescribeLoadBalancersPage />, qc)

    expect(screen.getByText("lb-one")).toBeInTheDocument()
    expect(screen.getByText("lb-one.example")).toBeInTheDocument()
    expect(screen.getByText("application")).toBeInTheDocument()
    expect(screen.getByText("internet-facing")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Load Balancer" }),
    ).not.toBeDisabled()
  })

  it("shows empty state when no load balancers", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers"], { LoadBalancers: [] })

    renderWithClient(<DescribeLoadBalancersPage />, qc)

    expect(screen.getByText("No load balancers found.")).toBeInTheDocument()
  })
})
