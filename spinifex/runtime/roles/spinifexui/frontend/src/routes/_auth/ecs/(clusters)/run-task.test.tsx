import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { routerState } = vi.hoisted(() => ({
  routerState: { navigate: vi.fn() },
}))

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

  return await mockRouter(importOriginal, { navigate: routerState.navigate })
})

import { RunTaskPage } from "./-components/run-task-page"

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

  return renderWithClient(<RunTaskPage cluster="web" />, qc)
}

describe("run-task route", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("runs a bridge-mode task without network config and navigates back", async () => {
    const user = userEvent.setup()
    sdk.setHandler("RunTaskCommand", () => ({ tasks: [{ taskArn: "arn:t" }] }))

    setup()

    await user.click(await screen.findByLabelText("Task definition"))
    await user.click(await screen.findByRole("option", { name: "app:1" }))
    await user.click(screen.getByRole("button", { name: "Run Task" }))

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
      expect(routerState.navigate).toHaveBeenCalledWith({
        to: "/ecs/list-clusters/$clusterName",
        params: { clusterName: "web" },
      })
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
    renderWithClient(<RunTaskPage cluster="web" />, qc)

    await user.click(await screen.findByLabelText("Task definition"))
    await user.click(await screen.findByRole("option", { name: "app:1" }))

    // Submit is disabled until a subnet is chosen.
    await screen.findByText(/awsvpc networking/)
    await user.click(screen.getByText(/subnet-aaa/))
    await user.click(screen.getByRole("button", { name: "Run Task" }))

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

  it("surfaces a placement failure and does not navigate", async () => {
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
    await user.click(screen.getByRole("button", { name: "Run Task" }))

    await expect(
      screen.findByText(/RESOURCE:placement/),
    ).resolves.toBeInTheDocument()
    expect(routerState.navigate).not.toHaveBeenCalled()
  })

  it("blocks submit when no task definition is selected", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByRole("button", { name: "Run Task" }))

    await expect(
      screen.findByText("Task definition is required"),
    ).resolves.toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
  })
})
