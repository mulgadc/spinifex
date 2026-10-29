import {
  type DBSubnetGroup,
  DeleteDBSubnetGroupCommand,
} from "@aws-sdk/client-rds"
import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
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

  it("deletes each selected DB subnet group with its own call", async () => {
    mockSend.mockClear()
    renderPage([GROUP, { ...GROUP, DBSubnetGroupName: "users-subnets" }])
    const user = userEvent.setup()

    await user.click(
      screen.getByRole("checkbox", { name: "Select orders-subnets" }),
    )
    await user.click(
      screen.getByRole("checkbox", { name: "Select users-subnets" }),
    )
    await user.click(screen.getByRole("button", { name: "Delete" }))

    const dialog = screen.getByRole("alertdialog")

    expect(
      within(dialog).getByText("Delete 2 DB subnet groups?"),
    ).toBeInTheDocument()
    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    const deletes = () =>
      mockSend.mock.calls
        .map(([command]) => command as unknown)
        .filter((command) => command instanceof DeleteDBSubnetGroupCommand)
        .map((command) => command.input.DBSubnetGroupName)

    await waitFor(() => {
      expect(deletes()).toStrictEqual(["orders-subnets", "users-subnets"])
    })
  })
})
