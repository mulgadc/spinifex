import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn())
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { createTestQueryClient, renderWithClient } from "@/test/render"

import { ClustersListPage } from "./clusters-list-page"

function renderPage(
  clusters: string[],
  search: Partial<PanelSearch> = {},
  onSearchChange = vi.fn(),
) {
  const qc = createTestQueryClient()

  for (const name of clusters) {
    qc.setQueryData(["eks", "clusters", name], {
      cluster: { name, status: "ACTIVE", version: "1.30" },
    })
  }

  renderWithClient(
    <ClustersListPage
      clusters={clusters}
      onSearchChange={onSearchChange}
      search={{ ...CLOSED_PANEL, ...search }}
    />,
    qc,
  )

  return onSearchChange
}

describe("ClustersListPage", () => {
  it("lists clusters sorted by name with each one's status and version", () => {
    renderPage(["zebra", "alpha"])
    const links = screen.getAllByRole("link", { name: /zebra|alpha/u })
    expect(links.map((l) => l.textContent)).toStrictEqual(["alpha", "zebra"])
    expect(screen.getAllByText("ACTIVE")).toHaveLength(2)
    expect(screen.getAllByText("1.30")).toHaveLength(2)
  })

  it("shows empty state when no clusters", () => {
    renderPage([])
    expect(screen.getByText("No EKS clusters found.")).toBeInTheDocument()
  })

  it("selects a cluster when its row is clicked", () => {
    const onSearchChange = renderPage(["c1"])
    fireEvent.click(screen.getByRole("cell", { name: "ACTIVE" }))
    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "c1",
    })
  })

  it("opens the selected cluster's panel, linked to its own page", () => {
    renderPage(["c1"], { selected: "c1" })
    const panel = screen.getByRole("region", { name: "Cluster details" })
    expect(
      panel.querySelector('a[href="/eks/list-clusters/c1"]'),
    ).toBeInTheDocument()
  })

  it("ignores a selection naming no listed cluster", () => {
    renderPage(["c1"], { selected: "gone" })
    expect(
      screen.queryByRole("region", { name: "Cluster details" }),
    ).not.toBeInTheDocument()
  })
})
