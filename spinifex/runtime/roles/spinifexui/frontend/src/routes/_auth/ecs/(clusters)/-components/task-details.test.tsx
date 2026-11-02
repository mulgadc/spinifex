import type { Task } from "@aws-sdk/client-ecs"
import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn())
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import type { PanelTab } from "@/lib/panel-search"

import { TaskDetails } from "./task-details"

const TASK_ID = "abc123def456"
const TASK_ARN = `arn:aws:ecs:ap-southeast-2:123456789012:task/web/${TASK_ID}`

function makeTask(
  details: { name: string; value: string }[],
  extra: Partial<Task> = {},
): Task {
  return {
    taskArn: TASK_ARN,
    lastStatus: "RUNNING",
    desiredStatus: "RUNNING",
    taskDefinitionArn:
      "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:3",
    launchType: "EC2",
    group: "service:web-svc",
    attachments: [{ type: "ElasticNetworkInterface", details }],
    containers: [
      {
        name: "app",
        image: "nginx:latest",
        lastStatus: "RUNNING",
        healthStatus: "HEALTHY",
      },
    ],
    ...extra,
  }
}

function renderDetails(task: Task, tab: PanelTab = "overview") {
  renderWithClient(
    <TaskDetails
      clusterName="web"
      layout="page"
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      tab={tab}
      task={task}
    />,
    createTestQueryClient(),
  )
}

describe("TaskDetails", () => {
  it("shows private and public IP when both are present", () => {
    renderDetails(
      makeTask([
        { name: "networkInterfaceId", value: "eni-0abc" },
        { name: "privateIPv4Address", value: "10.0.1.5" },
        { name: "publicIPv4Address", value: "54.1.2.3" },
        { name: "subnetId", value: "subnet-123" },
        { name: "macAddress", value: "0a:1b:2c:3d:4e:5f" },
      ]),
    )

    expect(screen.getByText("Private IP")).toBeInTheDocument()
    expect(screen.getByText("10.0.1.5")).toBeInTheDocument()
    expect(screen.getByText("Public IP")).toBeInTheDocument()
    expect(screen.getByText("54.1.2.3")).toBeInTheDocument()
    expect(screen.getByText("eni-0abc")).toBeInTheDocument()
  })

  it("renders an em dash for an absent public IP", () => {
    renderDetails(
      makeTask([
        { name: "networkInterfaceId", value: "eni-0abc" },
        { name: "privateIPv4Address", value: "10.0.1.5" },
        { name: "subnetId", value: "subnet-123" },
      ]),
    )

    expect(screen.getByText("10.0.1.5")).toBeInTheDocument()
    expect(screen.getByText("Public IP")).toBeInTheDocument()
    expect(screen.queryByText("54.1.2.3")).not.toBeInTheDocument()
    expect(screen.getAllByText("—").length).toBeGreaterThan(0)
  })

  it("lists containers with status and health", () => {
    renderDetails(makeTask([{ name: "privateIPv4Address", value: "10.0.1.5" }]))

    expect(screen.getByText("app")).toBeInTheDocument()
    expect(screen.getByText("nginx:latest")).toBeInTheDocument()
    expect(screen.getByText("HEALTHY")).toBeInTheDocument()
  })

  it("shows a container's GPU device UUIDs when gpuIds is present", () => {
    renderDetails(
      makeTask([], {
        containers: [
          {
            name: "app",
            image: "nginx:latest",
            lastStatus: "RUNNING",
            gpuIds: ["GPU-abc123", "GPU-def456"],
          },
        ],
      }),
    )

    expect(screen.getByText("GPUs")).toBeInTheDocument()
    expect(screen.getByText("GPU-abc123, GPU-def456")).toBeInTheDocument()
  })

  it("omits GPUs when no container has gpuIds", () => {
    renderDetails(makeTask([]))

    expect(screen.queryByText("GPUs")).not.toBeInTheDocument()
  })

  it("confirms before stopping the task", () => {
    renderDetails(makeTask([]), "settings")

    fireEvent.click(screen.getByRole("button", { name: "Stop task" }))
    expect(screen.getByText(/releases its ENI/)).toBeInTheDocument()
  })

  it("keeps Stop visible but disabled on a stopped task", () => {
    renderDetails(makeTask([], { lastStatus: "STOPPED" }), "settings")

    expect(screen.getByRole("button", { name: "Stop task" })).toBeDisabled()
  })
})
