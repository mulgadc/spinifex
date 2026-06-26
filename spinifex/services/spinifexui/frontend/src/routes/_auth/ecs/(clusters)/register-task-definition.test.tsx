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

import { RegisterTaskDefinitionPage } from "./-components/register-task-definition-page"

function setup() {
  return renderWithClient(
    <RegisterTaskDefinitionPage cluster="web" />,
    createTestQueryClient(),
  )
}

describe("register-task-definition route", () => {
  beforeEach(() => {
    sdk.reset()
    routerState.navigate.mockClear()
  })
  afterEach(() => vi.clearAllMocks())

  it("registers a single-container task definition and navigates back", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RegisterTaskDefinitionCommand", () => ({
      taskDefinition: { taskDefinitionArn: "arn:td:app:1" },
    }))

    setup()

    await user.type(await screen.findByLabelText("Family"), "app")
    await user.type(screen.getByLabelText("Name"), "web")
    await user.type(
      screen.getByLabelText("Image"),
      "public.ecr.aws/nginx/nginx:latest",
    )
    await user.click(
      screen.getByRole("button", { name: "Register Task Definition" }),
    )

    await waitFor(() => expect(sdk.send).toHaveBeenCalledOnce())
    const input = sdk.send.mock.calls[0]?.[0].input as {
      family: string
      networkMode: string
      containerDefinitions: { name: string; image: string }[]
    }
    expect(input.family).toBe("app")
    expect(input.networkMode).toBe("bridge")
    expect(input.containerDefinitions[0]?.name).toBe("web")
    expect(input.containerDefinitions[0]?.image).toBe(
      "public.ecr.aws/nginx/nginx:latest",
    )

    await waitFor(() => {
      expect(routerState.navigate).toHaveBeenCalledWith({
        to: "/ecs/list-clusters/$clusterName",
        params: { clusterName: "web" },
      })
    })
  })

  it("blocks submit and shows validation errors when required fields are empty", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(
      screen.getByRole("button", { name: "Register Task Definition" }),
    )

    await expect(
      screen.findByText("Family is required"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByText("Container name is required")).toBeInTheDocument()
    expect(screen.getByText("Image is required")).toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
  })

  it("includes port mappings when added", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RegisterTaskDefinitionCommand", () => ({
      taskDefinition: { taskDefinitionArn: "arn:td:app:1" },
    }))

    setup()

    await user.type(await screen.findByLabelText("Family"), "app")
    await user.type(screen.getByLabelText("Name"), "web")
    await user.type(screen.getByLabelText("Image"), "nginx")
    await user.click(screen.getByRole("button", { name: "Add port" }))
    await user.click(
      screen.getByRole("button", { name: "Register Task Definition" }),
    )

    await waitFor(() => expect(sdk.send).toHaveBeenCalledOnce())
    const input = sdk.send.mock.calls[0]?.[0].input as {
      containerDefinitions: {
        portMappings?: { containerPort: number; protocol: string }[]
      }[]
    }
    expect(input.containerDefinitions[0]?.portMappings?.[0]).toMatchObject({
      containerPort: 80,
      protocol: "tcp",
    })
  })
})
