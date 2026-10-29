import type { DBParameterGroup } from "@aws-sdk/client-rds"
import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { DBParameterGroupsListPage } from "./db-parameter-groups-list-page"

const CUSTOMER_GROUP = {
  DBParameterGroupName: "orders-pg",
  DBParameterGroupFamily: "postgres18",
  Description: "Tuned settings",
}

const DEFAULT_GROUP = {
  DBParameterGroupName: "default.postgres18",
  DBParameterGroupFamily: "postgres18",
  Description: "Default parameter group for postgres18",
}

function renderPage(
  groups: DBParameterGroup[],
  search: Partial<PanelSearch> = {},
  onSearchChange = vi.fn(),
) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "parameters", "orders-pg"], { Parameters: [] })
  renderWithClient(
    <DBParameterGroupsListPage
      groups={groups}
      onSearchChange={onSearchChange}
      search={{ ...CLOSED_PANEL, ...search }}
    />,
    qc,
  )

  return onSearchChange
}

describe("DBParameterGroupsListPage", () => {
  it("renders each group with its family and type", () => {
    renderPage([CUSTOMER_GROUP, DEFAULT_GROUP])
    expect(screen.getByRole("link", { name: "orders-pg" })).toBeInTheDocument()
    expect(screen.getAllByText("postgres18")).toHaveLength(2)
    expect(screen.getByText("Default")).toBeInTheDocument()
    expect(screen.getByText("Customer")).toBeInTheDocument()
  })

  it("shows the empty state with no groups", () => {
    renderPage([])
    expect(
      screen.getByText("No DB parameter groups found."),
    ).toBeInTheDocument()
  })

  it("selects a group when its row is clicked", () => {
    const onSearchChange = renderPage([CUSTOMER_GROUP])
    fireEvent.click(screen.getByText("Tuned settings"))
    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "orders-pg",
    })
  })

  it("opens the selected group in the panel", () => {
    renderPage([CUSTOMER_GROUP], { selected: "orders-pg" })
    expect(
      screen.getByRole("region", { name: "DB parameter group details" }),
    ).toBeInTheDocument()
  })

  it("links the heading action to the create page", () => {
    renderPage([])
    expect(
      screen.getByRole("link", { name: "Create Parameter Group" }),
    ).toHaveAttribute("href", "/rds/create-db-parameter-group")
  })
})
