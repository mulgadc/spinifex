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

import { CreateServicePage } from "./create-service-page"

const TASK_DEF_ARN =
  "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:1"

function setup() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "task-definitions"], [TASK_DEF_ARN])
  qc.setQueryData(["ec2", "subnets"], { Subnets: [] })
  qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })
  qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: [] })
  qc.setQueryData(["ecs", "task-definitions", "app:1"], {
    networkMode: "bridge",
  })

  return renderWithClient(
    <CreateServicePage cluster="web" onClose={onClose} />,
    qc,
  )
}

async function cliText(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "AWS CLI" }))

  return screen.getByText(/AWS_PROFILE=spinifex/).closest("code")
}

describe("CreateServicePage", () => {
  beforeEach(() => {
    sdk.reset()
    onClose.mockReset()
  })

  it("creates a REPLICA service without a load balancer and closes the panel", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateServiceCommand", () => ({
      service: { serviceArn: "arn:s" },
    }))

    setup()

    await user.type(await screen.findByLabelText("Service name"), "api")
    await user.click(screen.getByLabelText("Task definition"))
    await user.click(await screen.findByRole("option", { name: "app:1" }))
    await user.click(screen.getByRole("button", { name: "Create service" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledOnce()
    })

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
      expect(onClose).toHaveBeenCalledOnce()
    })
  })

  it("blocks submit and shows a validation error when the name is empty", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(
      await screen.findByRole("button", { name: "Create service" }),
    )

    await expect(
      screen.findByText("Service name is required"),
    ).resolves.toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
  })

  it("shows the create-service command with the typed name", async () => {
    const user = userEvent.setup()
    setup()

    await user.type(await screen.findByLabelText("Service name"), "api")

    const code = await cliText(user)

    expect(code).toHaveTextContent("aws ecs create-service")
    expect(code).toHaveTextContent("--service-name api")
    expect(code).toHaveTextContent("--task-definition <task-definition>")
    expect(code).not.toHaveTextContent("--load-balancers")
  })
})
