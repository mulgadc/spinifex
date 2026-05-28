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

import { TargetGroupDetailPage } from "../-components/target-group-detail-page"

const TG_ARN = "arn:tg:1"

describe("target-group detail route", () => {
  beforeEach(() => sdk.reset())
  afterEach(() => vi.clearAllMocks())

  function seed() {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "targetGroups", TG_ARN], {
      TargetGroups: [
        {
          TargetGroupArn: TG_ARN,
          TargetGroupName: "my-tg",
          Protocol: "HTTP",
          Port: 80,
          VpcId: "vpc-aaa",
          TargetType: "instance",
          HealthCheckEnabled: true,
          HealthCheckProtocol: "HTTP",
          HealthCheckPath: "/health",
          HealthCheckPort: "traffic-port",
          HealthCheckIntervalSeconds: 30,
          HealthCheckTimeoutSeconds: 5,
          HealthyThresholdCount: 5,
          UnhealthyThresholdCount: 2,
          Matcher: { HttpCode: "200" },
        },
      ],
    })
    qc.setQueryData(["elbv2", "targetGroups", TG_ARN, "attributes"], {
      Attributes: [
        {
          Key: "deregistration_delay.timeout_seconds",
          Value: "300",
        },
      ],
    })
    qc.setQueryData(["elbv2", "tags", TG_ARN], {
      TagDescriptions: [
        {
          ResourceArn: TG_ARN,
          Tags: [{ Key: "env", Value: "prod" }],
        },
      ],
    })
    qc.setQueryData(["ec2", "instances"], { Reservations: [] })
    return qc
  }

  it("renders the overview card with TG fields", () => {
    renderWithClient(<TargetGroupDetailPage arn={TG_ARN} />, seed())

    expect(screen.getByText("my-tg")).toBeInTheDocument()
    expect(screen.getByText("Target Group Details")).toBeInTheDocument()
    expect(screen.getByText(TG_ARN)).toBeInTheDocument()
    expect(screen.getByText("instance")).toBeInTheDocument()
  })

  it("renders a not-found state when the target group ARN is unknown", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "targetGroups", TG_ARN], { TargetGroups: [] })
    qc.setQueryData(["elbv2", "targetGroups", TG_ARN, "attributes"], {
      Attributes: [],
    })
    qc.setQueryData(["elbv2", "tags", TG_ARN], { TagDescriptions: [] })

    renderWithClient(<TargetGroupDetailPage arn={TG_ARN} />, qc)

    expect(screen.getByText("Target group not found.")).toBeInTheDocument()
  })
})
