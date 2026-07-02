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
      throw new Error(`No handler for ${command.constructor.name}`)
    }
    return handler(command.input)
  })
  return {
    routerState: { navigate: vi.fn() },
    sdk: {
      send,
      setHandler: (name: string, handler: (input: unknown) => unknown) =>
        handlers.set(name, handler),
      reset: () => {
        handlers.clear()
        send.mockClear()
      },
    },
  }
})

vi.mock("@/lib/awsClient", () => ({
  getEcsClient: () => ({ send: sdk.send }),
  getEc2Client: () => ({ send: sdk.send }),
  getElbv2Client: () => ({ send: sdk.send }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => routerState.navigate,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { CreateServicePage } from "./-components/create-service-page"

const TASK_DEF_ARN =
  "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:1"

describe("create-service route", () => {
  beforeEach(() => {
    sdk.reset()
    routerState.navigate.mockClear()
  })
  afterEach(() => vi.clearAllMocks())

  function setup() {
    const qc = createTestQueryClient()
    qc.setQueryData(["ecs", "task-definitions"], [TASK_DEF_ARN])
    qc.setQueryData(["ec2", "subnets"], { Subnets: [] })
    qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })
    qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: [] })
    qc.setQueryData(["ecs", "task-definitions", "app:1"], {
      networkMode: "bridge",
    })
    return renderWithClient(<CreateServicePage cluster="web" />, qc)
  }

  it("creates a REPLICA service without a load balancer and navigates back", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateServiceCommand", () => ({
      service: { serviceArn: "arn:s" },
    }))

    setup()

    await user.type(await screen.findByLabelText("Service name"), "api")
    await user.click(screen.getByLabelText("Task definition"))
    await user.click(await screen.findByRole("option", { name: "app:1" }))
    await user.click(screen.getByRole("button", { name: "Create Service" }))

    await waitFor(() => expect(sdk.send).toHaveBeenCalledOnce())
    const input = sdk.send.mock.calls[0]?.[0].input as {
      cluster: string
      serviceName: string
      taskDefinition: string
      desiredCount: number
      networkConfiguration?: unknown
      loadBalancers?: unknown
    }
    expect(input.cluster).toBe("web")
    expect(input.serviceName).toBe("api")
    expect(input.taskDefinition).toBe("app:1")
    expect(input.desiredCount).toBe(1)
    expect(input.networkConfiguration).toBeUndefined()
    expect(input.loadBalancers).toBeUndefined()

    await waitFor(() => {
      expect(routerState.navigate).toHaveBeenCalledWith({
        to: "/ecs/list-clusters/$clusterName",
        params: { clusterName: "web" },
      })
    })
  })

  it("blocks submit and shows a validation error when the name is empty", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(
      await screen.findByRole("button", { name: "Create Service" }),
    )

    await expect(
      screen.findByText("Service name is required"),
    ).resolves.toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
  })
})
