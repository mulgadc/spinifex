import type { Cluster } from "@aws-sdk/client-ecs"
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

import { ClusterDetails } from "./cluster-details"

const CLUSTER: Cluster = {
  clusterName: "web",
  clusterArn: "arn:aws:ecs:ap-southeast-2:123456789012:cluster/web",
  status: "ACTIVE",
  activeServicesCount: 2,
  runningTasksCount: 3,
  pendingTasksCount: 0,
  registeredContainerInstancesCount: 1,
}

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "clusters", "web", "services"], [])
  qc.setQueryData(["ecs", "clusters", "web", "tasks"], [])
  qc.setQueryData(["ecs", "clusters", "web", "container-instances"], [])

  return qc
}

function renderDetails(
  cluster: Cluster = CLUSTER,
  tab: PanelTab = "overview",
  onClose = vi.fn(),
) {
  renderWithClient(
    <ClusterDetails
      cluster={cluster}
      layout="page"
      onClose={onClose}
      onTabChange={vi.fn()}
      tab={tab}
    />,
    seed(),
  )
}

describe("ClusterDetails", () => {
  it("shows the cluster's details and its services, tasks and instances", () => {
    renderDetails()

    expect(screen.getByRole("heading", { name: "web" })).toBeInTheDocument()
    expect(
      screen.getByText("arn:aws:ecs:ap-southeast-2:123456789012:cluster/web"),
    ).toBeInTheDocument()

    for (const section of [
      "Services (0)",
      "Tasks (0)",
      "Container instances (0)",
    ]) {
      expect(screen.getByRole("button", { name: section })).toBeInTheDocument()
    }

    expect(
      screen.getByRole("link", { name: "Create service" }),
    ).toHaveAttribute("href", "/ecs/create-service")
    expect(screen.getByRole("link", { name: "Run task" })).toHaveAttribute(
      "href",
      "/ecs/run-task",
    )
  })

  it("lists the cluster's tags in Settings, in EC2's shape", () => {
    renderDetails(
      { ...CLUSTER, tags: [{ key: "env", value: "prod" }] },
      "settings",
    )

    expect(screen.getByText("env")).toBeInTheDocument()
    expect(screen.getByText("prod")).toBeInTheDocument()
  })

  it("confirms before deleting the cluster", () => {
    renderDetails(CLUSTER, "settings")

    fireEvent.click(screen.getByRole("button", { name: "Delete cluster" }))
    expect(
      screen.getByText(/permanently deletes cluster "web"/),
    ).toBeInTheDocument()
  })
})
