import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getElbv2Client: () => ({ send: mockSend }),
  getEc2Client: () => ({ send: mockSend }),
}))

import { TargetsTab } from "./targets-tab"

const TG_ARN = "arn:aws:elasticloadbalancing:tg/app/foo/abc"

function createWrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    )
  }
}

function seedClient(opts: {
  instances?: unknown
  health?: unknown
}): QueryClient {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: Number.POSITIVE_INFINITY,
        refetchOnMount: false,
      },
      mutations: { retry: false },
    },
  })
  queryClient.setQueryData(
    ["ec2", "instances"],
    opts.instances ?? { Reservations: [] },
  )
  queryClient.setQueryData(
    ["elbv2", "targetGroups", TG_ARN, "health"],
    opts.health ?? { TargetHealthDescriptions: [] },
  )
  return queryClient
}

describe("TargetsTab", () => {
  afterEach(() => {
    mockSend.mockReset()
  })

  it("renders empty state when no targets registered", () => {
    const queryClient = seedClient({})
    render(
      <TargetsTab
        defaultPort={80}
        isActive={true}
        targetGroupArn={TG_ARN}
        vpcId="vpc-aaa"
      />,
      { wrapper: createWrapper(queryClient) },
    )
    expect(screen.getByText("No targets registered.")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Register targets" }),
    ).toBeInTheDocument()
  })

  it("renders a target health row with colored chip", () => {
    const queryClient = seedClient({
      health: {
        TargetHealthDescriptions: [
          {
            Target: { Id: "i-aaa", Port: 80, AvailabilityZone: "az-1" },
            TargetHealth: {
              State: "healthy",
              Reason: "",
              Description: "",
            },
          },
        ],
      },
    })
    render(
      <TargetsTab
        defaultPort={80}
        isActive={true}
        targetGroupArn={TG_ARN}
        vpcId="vpc-aaa"
      />,
      { wrapper: createWrapper(queryClient) },
    )
    expect(screen.getByText("i-aaa")).toBeInTheDocument()
    expect(screen.getByText("healthy")).toBeInTheDocument()
    expect(screen.getByText("az-1")).toBeInTheDocument()
  })

  it("opens deregister confirm dialog and calls DeregisterTargetsCommand", async () => {
    const queryClient = seedClient({
      health: {
        TargetHealthDescriptions: [
          {
            Target: { Id: "i-aaa", Port: 80 },
            TargetHealth: { State: "healthy" },
          },
        ],
      },
    })
    mockSend.mockResolvedValue({})
    const user = userEvent.setup()
    render(
      <TargetsTab
        defaultPort={80}
        isActive={true}
        targetGroupArn={TG_ARN}
        vpcId="vpc-aaa"
      />,
      { wrapper: createWrapper(queryClient) },
    )
    await waitFor(() =>
      expect(screen.getByLabelText("Deregister i-aaa")).toBeInTheDocument(),
    )
    await user.click(screen.getByLabelText("Deregister i-aaa"))
    expect(
      screen.getByText(/Deregister i-aaa:80 from this target group/),
    ).toBeInTheDocument()
    const deregCallsBefore = mockSend.mock.calls.length
    await user.click(screen.getByRole("button", { name: "Delete" }))
    await waitFor(() =>
      expect(mockSend.mock.calls.length).toBeGreaterThan(deregCallsBefore),
    )
    const deregCall = mockSend.mock.calls[deregCallsBefore]?.[0]
    expect(deregCall.input).toStrictEqual({
      TargetGroupArn: TG_ARN,
      Targets: [{ Id: "i-aaa", Port: 80 }],
    })
  })

  it("filters instances by vpcId in the register dialog", async () => {
    const queryClient = seedClient({
      instances: {
        Reservations: [
          {
            Instances: [
              { InstanceId: "i-in-vpc", VpcId: "vpc-aaa", Tags: [] },
              { InstanceId: "i-other-vpc", VpcId: "vpc-bbb", Tags: [] },
            ],
          },
        ],
      },
    })
    const user = userEvent.setup()
    render(
      <TargetsTab
        defaultPort={80}
        isActive={true}
        targetGroupArn={TG_ARN}
        vpcId="vpc-aaa"
      />,
      { wrapper: createWrapper(queryClient) },
    )
    await user.click(screen.getByRole("button", { name: "Register targets" }))
    expect(screen.getByText("i-in-vpc")).toBeInTheDocument()
    expect(screen.queryByText("i-other-vpc")).not.toBeInTheDocument()
  })
})
