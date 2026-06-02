import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getElbv2Client: () => ({ send: mockSend }),
}))

import { ListenersTab } from "./listeners-tab"

const LB_ARN = "arn:aws:elasticloadbalancing:lb/app/foo/abc"
const TG_ARN_A = "arn:aws:elasticloadbalancing:tg/app/a/aaa"
const TG_ARN_B = "arn:aws:elasticloadbalancing:tg/app/b/bbb"

function createWrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    )
  }
}

function seedClient(opts: {
  listeners?: unknown
  targetGroups?: unknown
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
    ["elbv2", "listeners", LB_ARN],
    opts.listeners ?? { Listeners: [] },
  )
  queryClient.setQueryData(
    ["elbv2", "targetGroups"],
    opts.targetGroups ?? { TargetGroups: [] },
  )
  return queryClient
}

describe("ListenersTab", () => {
  afterEach(() => {
    mockSend.mockReset()
  })

  it("renders empty state when no listeners", () => {
    const queryClient = seedClient({})
    render(<ListenersTab loadBalancerArn={LB_ARN} vpcId="vpc-aaa" />, {
      wrapper: createWrapper(queryClient),
    })
    expect(screen.getByText("No listeners configured.")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Add listener" }),
    ).toBeInTheDocument()
  })

  it("renders listener rows with resolved default target-group name", () => {
    const queryClient = seedClient({
      listeners: {
        Listeners: [
          {
            ListenerArn: "arn:listener/1",
            Protocol: "HTTP",
            Port: 80,
            DefaultActions: [{ Type: "forward", TargetGroupArn: TG_ARN_A }],
          },
        ],
      },
      targetGroups: {
        TargetGroups: [
          {
            TargetGroupArn: TG_ARN_A,
            TargetGroupName: "my-tg",
            Protocol: "HTTP",
            Port: 80,
            VpcId: "vpc-aaa",
          },
        ],
      },
    })
    render(<ListenersTab loadBalancerArn={LB_ARN} vpcId="vpc-aaa" />, {
      wrapper: createWrapper(queryClient),
    })
    expect(screen.getByText("forward → my-tg")).toBeInTheDocument()
    expect(screen.getByText("HTTP")).toBeInTheDocument()
    expect(screen.getByText("80")).toBeInTheDocument()
  })

  it("opens delete confirm and calls DeleteListenerCommand", async () => {
    const queryClient = seedClient({
      listeners: {
        Listeners: [
          {
            ListenerArn: "arn:listener/1",
            Protocol: "HTTP",
            Port: 80,
            DefaultActions: [{ Type: "forward", TargetGroupArn: TG_ARN_A }],
          },
        ],
      },
    })
    mockSend.mockResolvedValue({})
    const user = userEvent.setup()
    render(<ListenersTab loadBalancerArn={LB_ARN} vpcId="vpc-aaa" />, {
      wrapper: createWrapper(queryClient),
    })
    await user.click(screen.getByLabelText("Delete listener HTTP:80"))
    expect(screen.getByText(/delete listener http:80/i)).toBeInTheDocument()
    const before = mockSend.mock.calls.length
    await user.click(screen.getByRole("button", { name: "Delete" }))
    await waitFor(() =>
      expect(mockSend.mock.calls.length).toBeGreaterThan(before),
    )
    const call = mockSend.mock.calls[before]?.[0]
    expect(call.input).toStrictEqual({ ListenerArn: "arn:listener/1" })
  })

  it("filters target groups by vpcId in the add-listener dialog", async () => {
    const queryClient = seedClient({
      targetGroups: {
        TargetGroups: [
          {
            TargetGroupArn: TG_ARN_A,
            TargetGroupName: "tg-in-vpc",
            Protocol: "HTTP",
            Port: 80,
            VpcId: "vpc-aaa",
          },
          {
            TargetGroupArn: TG_ARN_B,
            TargetGroupName: "tg-other-vpc",
            Protocol: "HTTP",
            Port: 80,
            VpcId: "vpc-bbb",
          },
        ],
      },
    })
    const user = userEvent.setup()
    render(<ListenersTab loadBalancerArn={LB_ARN} vpcId="vpc-aaa" />, {
      wrapper: createWrapper(queryClient),
    })
    await user.click(screen.getByRole("button", { name: "Add listener" }))
    await user.click(screen.getByLabelText("Default target group"))
    await expect(screen.findByText(/tg-in-vpc/)).resolves.toBeInTheDocument()
    expect(screen.queryByText(/tg-other-vpc/)).not.toBeInTheDocument()
  })

  it("opens edit dialog pre-filled and calls ModifyListenerCommand", async () => {
    const queryClient = seedClient({
      listeners: {
        Listeners: [
          {
            ListenerArn: "arn:listener/1",
            LoadBalancerArn: LB_ARN,
            Protocol: "HTTP",
            Port: 80,
            DefaultActions: [{ Type: "forward", TargetGroupArn: TG_ARN_A }],
          },
        ],
      },
      targetGroups: {
        TargetGroups: [
          {
            TargetGroupArn: TG_ARN_A,
            TargetGroupName: "tg-a",
            Protocol: "HTTP",
            Port: 80,
            VpcId: "vpc-aaa",
          },
        ],
      },
    })
    mockSend.mockResolvedValue({})
    const user = userEvent.setup()
    render(<ListenersTab loadBalancerArn={LB_ARN} vpcId="vpc-aaa" />, {
      wrapper: createWrapper(queryClient),
    })
    await user.click(screen.getByLabelText("Edit listener HTTP:80"))
    await expect(
      screen.findByRole("alertdialog", { name: /edit listener/i }),
    ).resolves.toBeInTheDocument()
    const portInput = screen.getByLabelText<HTMLInputElement>(/^port$/i)
    expect(portInput.value).toBe("80")
    await user.clear(portInput)
    await user.type(portInput, "8080")
    const before = mockSend.mock.calls.length
    await user.click(screen.getByRole("button", { name: /save changes/i }))
    await waitFor(() =>
      expect(mockSend.mock.calls.length).toBeGreaterThan(before),
    )
    const call = mockSend.mock.calls[before]?.[0]
    expect(call.input).toStrictEqual({
      ListenerArn: "arn:listener/1",
      Protocol: "HTTP",
      Port: 8080,
      DefaultActions: [{ Type: "forward", TargetGroupArn: TG_ARN_A }],
    })
  })

  it("submits add-listener form and calls CreateListenerCommand", async () => {
    const queryClient = seedClient({
      targetGroups: {
        TargetGroups: [
          {
            TargetGroupArn: TG_ARN_A,
            TargetGroupName: "tg-a",
            Protocol: "HTTP",
            Port: 80,
            VpcId: "vpc-aaa",
          },
        ],
      },
    })
    mockSend.mockResolvedValue({})
    const user = userEvent.setup()
    render(<ListenersTab loadBalancerArn={LB_ARN} vpcId="vpc-aaa" />, {
      wrapper: createWrapper(queryClient),
    })
    await user.click(screen.getByRole("button", { name: "Add listener" }))
    await user.click(screen.getByLabelText("Default target group"))
    await user.click(await screen.findByText(/tg-a/))
    const before = mockSend.mock.calls.length
    await user.click(screen.getByRole("button", { name: /^add listener$/i }))
    await waitFor(() =>
      expect(mockSend.mock.calls.length).toBeGreaterThan(before),
    )
    const call = mockSend.mock.calls[before]?.[0]
    expect(call.input).toStrictEqual({
      LoadBalancerArn: LB_ARN,
      Protocol: "HTTP",
      Port: 80,
      DefaultActions: [{ Type: "forward", TargetGroupArn: TG_ARN_A }],
    })
  })
})
