import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/awsClient", () => ({
  getEcsClient: () => ({ send: vi.fn() }),
}))

vi.mock("@tanstack/react-router", async (orig) => ({
  ...(await orig<typeof import("@tanstack/react-router")>()),
  Link: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}))

import { TaskDefinitionsTab } from "./task-definitions-tab"

function seed(arns: string[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "task-definitions"], arns)
  return qc
}

describe("TaskDefinitionsTab", () => {
  it("renders family:revision from the ARN with a deregister action", () => {
    renderWithClient(
      <TaskDefinitionsTab clusterName="web" />,
      seed(["arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:3"]),
    )
    expect(screen.getByText("app:3")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Deregister" }),
    ).toBeInTheDocument()
  })

  it("opens the deregister confirmation", () => {
    renderWithClient(
      <TaskDefinitionsTab clusterName="web" />,
      seed(["arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:3"]),
    )
    fireEvent.click(screen.getByRole("button", { name: "Deregister" }))
    expect(
      screen.getByText(/marks task definition "app:3" INACTIVE/),
    ).toBeInTheDocument()
  })

  it("shows empty state with no task definitions", () => {
    renderWithClient(<TaskDefinitionsTab clusterName="web" />, seed([]))
    expect(screen.getByText("No task definitions found.")).toBeInTheDocument()
  })
})
