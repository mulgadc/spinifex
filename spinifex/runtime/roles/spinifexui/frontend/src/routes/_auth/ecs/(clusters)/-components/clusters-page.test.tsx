import type { Cluster } from "@aws-sdk/client-ecs"
import { fireEvent, screen, within } from "@testing-library/react"
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

// Wide enough for the panel to sit over the list rather than replace it.
vi.mock("@/hooks/use-wide-layout", () => ({ useWideLayout: () => true }))

import { CLOSED_PANEL } from "@/lib/panel-search"

import { ClustersPage } from "./clusters-page"

const CLUSTER: Cluster = {
  clusterName: "web",
  clusterArn: "arn:aws:ecs:ap-southeast-2:123456789012:cluster/web",
  status: "ACTIVE",
  activeServicesCount: 2,
  runningTasksCount: 3,
  registeredContainerInstancesCount: 1,
}

function renderPage({
  clusters = [CLUSTER],
  hasSystemImage = true,
  selected = "",
} = {}) {
  const onSearchChange = vi.fn()
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "clusters", "web", "services"], [])
  qc.setQueryData(["ecs", "clusters", "web", "tasks"], [])
  qc.setQueryData(["ecs", "clusters", "web", "container-instances"], [])

  renderWithClient(
    <ClustersPage
      clusters={clusters}
      hasSystemImage={hasSystemImage}
      onSearchChange={onSearchChange}
      search={{ ...CLOSED_PANEL, selected }}
    />,
    qc,
  )

  return { onSearchChange }
}

describe("ClustersPage", () => {
  it("renders cluster rows, and a row click opens its panel", () => {
    const { onSearchChange } = renderPage()

    expect(screen.getByRole("link", { name: "web" })).toBeInTheDocument()
    expect(screen.getByText("ACTIVE")).toBeInTheDocument()

    fireEvent.click(screen.getByRole("cell", { name: "ACTIVE" }))
    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "web",
    })
  })

  it("shows the selected cluster in a panel over the list", () => {
    renderPage({ selected: "web" })

    expect(screen.getByRole("table")).toBeInTheDocument()

    const panel = screen.getByRole("region", { name: "Cluster details" })

    const hrefs = within(panel)
      .getAllByRole("link")
      .map((link) => link.getAttribute("href"))

    expect(hrefs).toContain("/ecs/list-clusters/web")
  })

  it("shows empty state with no clusters", () => {
    renderPage({ clusters: [] })
    expect(screen.getByText("No ECS clusters found.")).toBeInTheDocument()
  })

  it("opens the create dialog when an ECS node image is present", () => {
    renderPage({ clusters: [] })
    fireEvent.click(screen.getByRole("button", { name: "Create cluster" }))
    expect(screen.getByText(/A cluster groups/)).toBeInTheDocument()
  })

  it("blocks creation and warns when no ECS node image exists", () => {
    renderPage({ clusters: [], hasSystemImage: false })
    expect(screen.getByText("ECS system image not found")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create cluster" }),
    ).toBeDisabled()
  })
})
