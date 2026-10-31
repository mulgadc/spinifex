import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

const onClose = vi.fn()

import { RegisterTaskDefinitionPage } from "./register-task-definition-page"

function setup() {
  return renderWithClient(
    <RegisterTaskDefinitionPage onClose={onClose} />,
    createTestQueryClient(),
  )
}

async function cliText(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "AWS CLI" }))

  return screen.getByText(/AWS_PROFILE=spinifex/).closest("code")
}

describe("RegisterTaskDefinitionPage", () => {
  beforeEach(() => {
    sdk.reset()
    onClose.mockReset()
  })

  it("registers a single-container task definition and closes with the family", async () => {
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
      screen.getByRole("button", { name: "Register task definition" }),
    )

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledOnce()
    })

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
      expect(onClose).toHaveBeenCalledWith("app")
    })
  })

  it("keeps the form open when registration is refused", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RegisterTaskDefinitionCommand", () => {
      throw new Error("family is invalid")
    })

    setup()

    await user.type(await screen.findByLabelText("Family"), "app")
    await user.type(screen.getByLabelText("Name"), "web")
    await user.type(screen.getByLabelText("Image"), "nginx")
    await user.click(
      screen.getByRole("button", { name: "Register task definition" }),
    )

    await expect(
      screen.findByText(/family is invalid/),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
  })

  it("blocks submit and shows validation errors when required fields are empty", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(
      screen.getByRole("button", { name: "Register task definition" }),
    )

    await expect(
      screen.findByText("Family is required"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByText("Container name is required")).toBeInTheDocument()
    expect(screen.getByText("Image is required")).toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
  })

  it("omits resourceRequirements when GPU count is 0", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RegisterTaskDefinitionCommand", () => ({
      taskDefinition: { taskDefinitionArn: "arn:td:app:1" },
    }))

    setup()

    await user.type(await screen.findByLabelText("Family"), "app")
    await user.type(screen.getByLabelText("Name"), "web")
    await user.type(screen.getByLabelText("Image"), "nginx")
    await user.click(
      screen.getByRole("button", { name: "Register task definition" }),
    )

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledOnce()
    })

    const input = sdk.send.mock.calls[0]?.[0].input as {
      containerDefinitions: { resourceRequirements?: unknown }[]
    }

    expect(input.containerDefinitions[0]?.resourceRequirements).toBeUndefined()
  })

  it("emits a GPU resourceRequirement for a non-zero GPU count", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RegisterTaskDefinitionCommand", () => ({
      taskDefinition: { taskDefinitionArn: "arn:td:app:1" },
    }))

    setup()

    await user.type(await screen.findByLabelText("Family"), "app")
    await user.type(screen.getByLabelText("Name"), "web")
    await user.type(screen.getByLabelText("Image"), "nginx")
    const gpuCount = screen.getByLabelText("GPU count")
    await user.clear(gpuCount)
    await user.type(gpuCount, "2")
    await user.click(
      screen.getByRole("button", { name: "Register task definition" }),
    )

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledOnce()
    })

    const input = sdk.send.mock.calls[0]?.[0].input as {
      containerDefinitions: {
        resourceRequirements?: { type: string; value: string }[]
      }[]
    }

    expect(input.containerDefinitions[0]?.resourceRequirements).toStrictEqual([
      { type: "GPU", value: "2" },
    ])
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
      screen.getByRole("button", { name: "Register task definition" }),
    )

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledOnce()
    })

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

  it("shows the register command with the container as JSON", async () => {
    const user = userEvent.setup()
    setup()

    await user.type(await screen.findByLabelText("Family"), "app")
    await user.type(screen.getByLabelText("Name"), "web")
    await user.type(screen.getByLabelText("Image"), "nginx")
    await user.type(screen.getByLabelText("Task memory (MiB)"), "512")

    const code = await cliText(user)

    expect(code).toHaveTextContent("--family app")
    expect(code).toHaveTextContent("--network-mode bridge")
    expect(code).toHaveTextContent("--memory 512")
    expect(code).not.toHaveTextContent("--cpu")
    expect(code).toHaveTextContent(
      `--container-definitions '[{"name":"web","image":"nginx","essential":true}]'`,
    )
  })
})
