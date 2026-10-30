import { DeleteClusterCommand } from "@aws-sdk/client-eks"
import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
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

  it("opens the create form in the panel, with the image callout inside it", async () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["ec2", "vpcs"], { Vpcs: [] })
    qc.setQueryData(["ec2", "subnets"], { Subnets: [] })
    qc.setQueryData(["iam", "roles"], { Roles: [] })
    qc.setQueryData(["ec2", "images"], { Images: [] })

    renderWithClient(
      <ClustersListPage
        clusters={[]}
        onSearchChange={vi.fn()}
        search={{ ...CLOSED_PANEL, create: true }}
      />,
      qc,
    )

    const panel = await screen.findByRole("region", { name: "Create cluster" })

    expect(
      within(panel).getByText("EKS system image not found"),
    ).toBeInTheDocument()
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

  it("deletes each selected cluster with its own call", async () => {
    mockSend.mockReset()
    mockSend.mockResolvedValue({})
    renderPage(["alpha", "zebra"])
    const user = userEvent.setup()

    await user.click(screen.getByRole("checkbox", { name: "Select alpha" }))
    await user.click(screen.getByRole("checkbox", { name: "Select zebra" }))
    await user.click(screen.getByRole("button", { name: "Delete" }))

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("Delete 2 clusters?")).toBeInTheDocument()
    await user.type(
      await screen.findByLabelText(/^To confirm, type/u),
      "delete",
    )
    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    // Each delete invalidates the cluster queries, so refetches interleave.
    const deletes = () =>
      mockSend.mock.calls
        .map(([command]) => command as unknown)
        .filter((command) => command instanceof DeleteClusterCommand)
        .map((command) => command.input.name)

    await waitFor(() => {
      expect(deletes()).toStrictEqual(["alpha", "zebra"])
    })
  })
})
