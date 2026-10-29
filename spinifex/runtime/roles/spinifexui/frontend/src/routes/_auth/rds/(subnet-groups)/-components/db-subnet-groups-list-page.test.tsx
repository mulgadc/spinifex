import type { DBSubnetGroup } from "@aws-sdk/client-rds"
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

import { DBSubnetGroupsListPage } from "./db-subnet-groups-list-page"

const GROUP = {
  DBSubnetGroupName: "orders-subnets",
  DBSubnetGroupDescription: "Private subnets",
  VpcId: "vpc-1",
  SubnetGroupStatus: "Complete",
  Subnets: [{ SubnetIdentifier: "subnet-1" }, { SubnetIdentifier: "subnet-2" }],
}

function renderPage(
  groups: DBSubnetGroup[],
  search: Partial<PanelSearch> = {},
  onSearchChange = vi.fn(),
) {
  renderWithClient(
    <DBSubnetGroupsListPage
      groups={groups}
      onSearchChange={onSearchChange}
      search={{ ...CLOSED_PANEL, ...search }}
    />,
    createTestQueryClient(),
  )

  return onSearchChange
}

describe("DBSubnetGroupsListPage", () => {
  it("renders a subnet group row", () => {
    renderPage([GROUP])
    expect(
      screen.getByRole("link", { name: "orders-subnets" }),
    ).toBeInTheDocument()
    expect(screen.getByText("vpc-1")).toBeInTheDocument()
    expect(screen.getByText("2")).toBeInTheDocument()
    expect(screen.getByText("Private subnets")).toBeInTheDocument()
  })

  it("explains the fallback when no group exists", () => {
    renderPage([])
    expect(screen.getByText(/No DB subnet groups found/)).toBeInTheDocument()
  })

  it("selects a group when its row is clicked", () => {
    const onSearchChange = renderPage([GROUP])
    fireEvent.click(screen.getByText("Private subnets"))
    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "orders-subnets",
    })
  })

  it("opens the selected group in the panel", () => {
    renderPage([GROUP], { selected: "orders-subnets" })
    expect(
      screen.getByRole("region", { name: "DB subnet group details" }),
    ).toBeInTheDocument()
  })

  it("links the heading action to the create page", () => {
    renderPage([])
    expect(
      screen.getByRole("link", { name: "Create Subnet Group" }),
    ).toHaveAttribute("href", "/rds/create-db-subnet-group")
  })
})
