import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen, waitFor } from "@testing-library/react"
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

import { DescribeTargetGroupsPage } from "../-components/describe-target-groups-page"

const TGS: TargetGroup[] = [
  {
    TargetGroupArn: "arn:tg:1",
    TargetGroupName: "tg-one",
    Protocol: "HTTP",
    Port: 80,
    VpcId: "vpc-aaa",
    TargetType: "instance",
  },
  {
    TargetGroupArn: "arn:tg:2",
    TargetGroupName: "tg-two",
    Protocol: "HTTP",
    Port: 8080,
    VpcId: "vpc-aaa",
    TargetType: "instance",
  },
]

describe("describe-target-groups list route", () => {
  beforeEach(() => sdk.reset())
  afterEach(() => vi.clearAllMocks())

  it("renders target-group rows with health summaries from the mocked SDK", async () => {
    sdk.setHandler("DescribeTargetHealthCommand", (input) => {
      const arn = (input as { TargetGroupArn: string }).TargetGroupArn
      if (arn === "arn:tg:1") {
        return {
          TargetHealthDescriptions: [
            { TargetHealth: { State: "healthy" } },
            { TargetHealth: { State: "unhealthy" } },
          ],
        }
      }
      return { TargetHealthDescriptions: [] }
    })

    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: TGS })

    renderWithClient(<DescribeTargetGroupsPage />, qc)

    expect(screen.getByText("tg-one")).toBeInTheDocument()
    expect(screen.getByText("tg-two")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Target Group" }),
    ).toBeInTheDocument()

    await waitFor(() => {
      expect(screen.getByText("1 healthy, 1 unhealthy")).toBeInTheDocument()
    })
  })

  it("shows empty state when there are no target groups", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: [] })

    renderWithClient(<DescribeTargetGroupsPage />, qc)

    expect(screen.getByText("No target groups found.")).toBeInTheDocument()
  })
})
