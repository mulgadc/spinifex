import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { routerState } = vi.hoisted(() => ({
  routerState: { navigate: vi.fn() },
}))

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => routerState.navigate,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
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

function seed(groups: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "parameterGroups"], { DBParameterGroups: groups })
  return qc
}

describe("DBParameterGroupsListPage", () => {
  it("renders a group with its family", () => {
    renderWithClient(
      <DBParameterGroupsListPage />,
      seed([CUSTOMER_GROUP, DEFAULT_GROUP]),
    )
    expect(screen.getByRole("link", { name: "orders-pg" })).toBeInTheDocument()
    expect(screen.getAllByText("postgres18")).toHaveLength(2)
  })

  // A default group has no stored record, so a delete would always be refused.
  it("marks the default group and refuses to offer its delete", () => {
    renderWithClient(
      <DBParameterGroupsListPage />,
      seed([CUSTOMER_GROUP, DEFAULT_GROUP]),
    )
    expect(screen.getByText("Default")).toBeInTheDocument()
    expect(screen.getByText("Customer")).toBeInTheDocument()

    const deleteButtons = screen.getAllByRole("button", { name: "Delete" })
    expect(deleteButtons[0]).toBeEnabled()
    expect(deleteButtons[1]).toBeDisabled()
  })

  it("shows the empty state with no groups", () => {
    renderWithClient(<DBParameterGroupsListPage />, seed([]))
    expect(
      screen.getByText("No DB parameter groups found."),
    ).toBeInTheDocument()
  })

  it("names the in-use refusal in the delete dialog", () => {
    renderWithClient(<DBParameterGroupsListPage />, seed([CUSTOMER_GROUP]))
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    expect(
      screen.getByText(/refused while any DB instance references the group/),
    ).toBeInTheDocument()
  })

  it("navigates to the create page from the heading action", () => {
    renderWithClient(<DBParameterGroupsListPage />, seed([]))
    fireEvent.click(
      screen.getByRole("button", { name: "Create parameter group" }),
    )
    expect(routerState.navigate).toHaveBeenCalledWith({
      to: "/rds/create-db-parameter-group",
    })
  })
})
