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

import { RunTaskPage } from "./run-task-page"
import { AWSVPC_SUBNET_REQUIRED } from "./task-launch-fields"

const TASK_DEF_ARN =
  "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:1"

function setup() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "task-definitions"], [TASK_DEF_ARN])
  qc.setQueryData(["ec2", "subnets"], { Subnets: [] })
  qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })
  qc.setQueryData(["ecs", "task-definitions", "app:1"], {
    networkMode: "bridge",
  })

  return renderWithClient(<RunTaskPage cluster="web" onClose={onClose} />, qc)
}

async function cliText(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "AWS CLI" }))

  return screen.getByText(/AWS_PROFILE=spinifex/).closest("code")
}

describe("RunTaskPage", () => {
  beforeEach(() => {
    sdk.reset()
    onClose.mockReset()
  })

  it("runs a bridge-mode task without network config and closes the panel", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RunTaskCommand", () => ({ tasks: [{ taskArn: "arn:t" }] }))

    setup()

    await user.click(await screen.findByLabelText("Task definition"))
    await user.click(await screen.findByRole("option", { name: "app:1" }))
    await user.click(screen.getByRole("button", { name: "Run task" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledOnce()
    })

    const input = sdk.send.mock.calls[0]?.[0].input as {
      cluster: string
      taskDefinition: string
      count: number
      networkConfiguration?: unknown
    }

    expect(input.cluster).toBe("web")
    expect(input.taskDefinition).toBe("app:1")
    expect(input.count).toBe(1)
    expect(input.networkConfiguration).toBeUndefined()

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledOnce()
    })
  })

  it("requires a subnet for an awsvpc task definition", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RunTaskCommand", () => ({ tasks: [{ taskArn: "arn:t" }] }))

    const qc = createTestQueryClient()
    qc.setQueryData(["ecs", "task-definitions"], [TASK_DEF_ARN])
    qc.setQueryData(["ec2", "subnets"], {
      Subnets: [{ SubnetId: "subnet-aaa", CidrBlock: "10.0.0.0/24" }],
    })
    qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })
    qc.setQueryData(["ecs", "task-definitions", "app:1"], {
      networkMode: "awsvpc",
    })
    renderWithClient(<RunTaskPage cluster="web" onClose={onClose} />, qc)

    await user.click(await screen.findByLabelText("Task definition"))
    await user.click(await screen.findByRole("option", { name: "app:1" }))

    await user.click(screen.getByRole("button", { name: "Run task" }))

    await expect(
      screen.findByText(AWSVPC_SUBNET_REQUIRED),
    ).resolves.toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()

    await user.click(screen.getByRole("switch", { name: "Subnet subnet-aaa" }))
    await user.click(screen.getByRole("button", { name: "Run task" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledOnce()
    })

    const input = sdk.send.mock.calls[0]?.[0].input as {
      networkConfiguration?: {
        awsvpcConfiguration?: { subnets?: string[] }
      }
    }

    expect(
      input.networkConfiguration?.awsvpcConfiguration?.subnets,
    ).toStrictEqual(["subnet-aaa"])
  })

  it("surfaces a placement failure and keeps the panel open", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RunTaskCommand", () => ({
      tasks: [],
      failures: [
        {
          reason: "RESOURCE:placement",
          detail: "no container instance has capacity for the task",
        },
      ],
    }))

    setup()

    await user.click(await screen.findByLabelText("Task definition"))
    await user.click(await screen.findByRole("option", { name: "app:1" }))
    await user.click(screen.getByRole("button", { name: "Run task" }))

    await expect(
      screen.findByText(/RESOURCE:placement/),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
  })

  it("blocks submit when no task definition is selected", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByRole("button", { name: "Run task" }))

    await expect(
      screen.findByText("Task definition is required"),
    ).resolves.toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
  })

  it("shows the run-task command for the chosen task definition", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByLabelText("Task definition"))
    await user.click(await screen.findByRole("option", { name: "app:1" }))
    const count = screen.getByLabelText("Count")
    await user.clear(count)
    await user.type(count, "3")

    const code = await cliText(user)

    expect(code).toHaveTextContent("aws ecs run-task")
    expect(code).toHaveTextContent("--cluster web")
    expect(code).toHaveTextContent("--task-definition app:1")
    expect(code).toHaveTextContent("--count 3")
    expect(code).not.toHaveTextContent("--network-configuration")
  })
})
