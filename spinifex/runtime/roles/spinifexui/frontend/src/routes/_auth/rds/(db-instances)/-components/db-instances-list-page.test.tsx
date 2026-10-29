import type { DBInstance } from "@aws-sdk/client-rds"
import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn().mockResolvedValue({}))
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { DBInstancesListPage } from "./db-instances-list-page"

const AVAILABLE_INSTANCE = {
  DBInstanceIdentifier: "orders-db",
  DBInstanceStatus: "available",
  Engine: "postgres",
  EngineVersion: "18",
  DBInstanceClass: "db.t3.micro",
  AllocatedStorage: 20,
  DeletionProtection: false,
  Endpoint: { Address: "orders-db.rds.internal", Port: 5432 },
}

function renderList(
  instances: DBInstance[],
  search: Partial<PanelSearch> = {},
  onSearchChange = vi.fn(),
) {
  renderWithClient(
    <DBInstancesListPage
      instances={instances}
      onSearchChange={onSearchChange}
      search={{ ...CLOSED_PANEL, ...search }}
    />,
    createTestQueryClient(),
  )

  return onSearchChange
}

describe("DBInstancesListPage", () => {
  it("renders a DB instance row", () => {
    renderList([AVAILABLE_INSTANCE])
    expect(screen.getByRole("link", { name: "orders-db" })).toBeInTheDocument()
    expect(screen.getByText("postgres 18")).toBeInTheDocument()
    expect(screen.getByText("db.t3.micro")).toBeInTheDocument()
    expect(screen.getByText("available")).toBeInTheDocument()
    expect(screen.getByText("orders-db.rds.internal")).toBeInTheDocument()
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
  })

  it("shows the empty state with no instances", () => {
    renderList([])
    expect(screen.getByText("No DB instances found.")).toBeInTheDocument()
  })

  it("selects the instance a row click opens", () => {
    const onSearchChange = renderList([AVAILABLE_INSTANCE])
    fireEvent.click(screen.getByText("db.t3.micro"))
    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "orders-db",
    })
  })

  it("renders the selected instance's details", () => {
    renderList([AVAILABLE_INSTANCE], { selected: "orders-db" })
    expect(
      screen.getByRole("region", { name: "Database details" }),
    ).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /Stop/u })).toBeEnabled()
  })

  it("opens no panel for an instance that is not in the list", () => {
    renderList([AVAILABLE_INSTANCE], { selected: "gone-db" })
    expect(
      screen.queryByRole("region", { name: "Database details" }),
    ).toBeNull()
  })

  it("links the heading action to the create page", () => {
    renderList([])
    expect(
      screen.getByRole("link", { name: "Create database" }),
    ).toHaveAttribute("href", "/rds/create-db-instance")
  })
})
