import { screen } from "@testing-library/react"
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

import { ClusterServices } from "./cluster-services"

function seed(services: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "clusters", "web", "services"], services)

  return qc
}

describe("ClusterServices", () => {
  it("renders service rows with counts and short task definition", () => {
    renderWithClient(
      <ClusterServices clusterName="web" />,
      seed([
        {
          serviceName: "api",
          serviceArn: "arn:aws:ecs:ap-southeast-2:123456789012:service/web/api",
          status: "ACTIVE",
          desiredCount: 2,
          runningCount: 2,
          pendingCount: 0,
          taskDefinition:
            "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:3",
        },
      ]),
    )
    expect(screen.getByRole("link", { name: "api" })).toHaveAttribute(
      "href",
      "/ecs/list-clusters/web/services/api",
    )
    expect(screen.getByText("ACTIVE")).toBeInTheDocument()
    expect(
      screen.getByText(/2 running of 2 desired.*app:3/u),
    ).toBeInTheDocument()
  })

  it("shows empty state with no services", () => {
    renderWithClient(<ClusterServices clusterName="web" />, seed([]))
    expect(screen.getByText("No services found.")).toBeInTheDocument()
  })
})
