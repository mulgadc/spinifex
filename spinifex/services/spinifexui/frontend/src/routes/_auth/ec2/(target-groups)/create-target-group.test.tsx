import type { Vpc } from "@aws-sdk/client-ec2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
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
    routerState: {
      navigate: vi.fn(),
      params: {} as Record<string, string>,
    },
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
    createFileRoute: () => (options: Record<string, unknown>) => ({
      ...options,
      useParams: () => routerState.params,
    }),
    useNavigate: () => routerState.navigate,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { CreateTargetGroupPage } from "./-components/create-target-group-page"

const VPCS: Vpc[] = [{ VpcId: "vpc-aaa", CidrBlock: "10.0.0.0/16", Tags: [] }]

describe("create-target-group route", () => {
  beforeEach(() => {
    sdk.reset()
    routerState.navigate.mockClear()
  })
  afterEach(() => vi.clearAllMocks())

  function setup() {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(["ec2", "vpcs"], { Vpcs: VPCS })
    return renderWithClient(<CreateTargetGroupPage />, queryClient)
  }

  it("creates a target group and navigates to the detail page on submit", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateTargetGroupCommand", () => ({
      TargetGroups: [{ TargetGroupArn: "arn:tg:new" }],
    }))

    setup()

    expect(await screen.findByLabelText("Name")).toBeInTheDocument()
    await user.type(screen.getByLabelText("Name"), "my-tg")
    await user.click(
      screen.getByRole("button", { name: "Create target group" }),
    )

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledTimes(1)
    })
    const input = sdk.send.mock.calls[0]?.[0].input as {
      Name: string
      Protocol: string
      VpcId: string
      TargetType: string
    }
    expect(input.Name).toBe("my-tg")
    expect(input.Protocol).toBe("HTTP")
    expect(input.VpcId).toBe("vpc-aaa")
    expect(input.TargetType).toBe("instance")

    await waitFor(() => {
      expect(routerState.navigate).toHaveBeenCalledWith({
        to: "/ec2/describe-target-groups/$id",
        params: { id: encodeURIComponent("arn:tg:new") },
      })
    })
  })

  it("blocks submit and shows a validation error when the name is empty", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(
      screen.getByRole("button", { name: "Create target group" }),
    )

    expect(await screen.findByText("Name is required")).toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
    expect(routerState.navigate).not.toHaveBeenCalled()
  })

  it("surfaces an error banner when the CreateTargetGroup call fails", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateTargetGroupCommand", () => {
      throw new Error("name already in use")
    })

    setup()

    await user.type(screen.getByLabelText("Name"), "my-tg")
    await user.click(
      screen.getByRole("button", { name: "Create target group" }),
    )

    expect(
      await screen.findByText(/Failed to create target group/i),
    ).toBeInTheDocument()
    expect(routerState.navigate).not.toHaveBeenCalled()
  })
})
