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

import { TasksTab } from "./tasks-tab"

function seed(tasks: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "clusters", "web", "tasks"], tasks)

  return qc
}

const RUNNING = {
  taskArn: "arn:aws:ecs:ap-southeast-2:123456789012:task/web/abc123",
  lastStatus: "RUNNING",
  desiredStatus: "RUNNING",
  group: "service:api",
}

describe("TasksTab", () => {
  it("renders running tasks with a stop action", () => {
    renderWithClient(<TasksTab clusterName="web" />, seed([RUNNING]))
    expect(screen.getByText("abc123")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Stop" })).toBeInTheDocument()
  })

  it("hides the stop action for stopped tasks", () => {
    renderWithClient(
      <TasksTab clusterName="web" />,
      seed([{ ...RUNNING, lastStatus: "STOPPED", desiredStatus: "STOPPED" }]),
    )
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull()
  })

  it("opens the stop confirmation", () => {
    renderWithClient(<TasksTab clusterName="web" />, seed([RUNNING]))
    fireEvent.click(screen.getByRole("button", { name: "Stop" }))
    expect(screen.getByText(/releases its ENI/)).toBeInTheDocument()
  })

  it("shows empty state with no tasks", () => {
    renderWithClient(<TasksTab clusterName="web" />, seed([]))
    expect(screen.getByText("No tasks found.")).toBeInTheDocument()
  })
})
