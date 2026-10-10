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

import { DBSubnetGroupsListPage } from "./db-subnet-groups-list-page"

const GROUP = {
  DBSubnetGroupName: "orders-subnets",
  DBSubnetGroupDescription: "Private subnets",
  VpcId: "vpc-1",
  SubnetGroupStatus: "Complete",
  Subnets: [{ SubnetIdentifier: "subnet-1" }, { SubnetIdentifier: "subnet-2" }],
}

function seed(groups: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "subnetGroups"], { DBSubnetGroups: groups })
  return qc
}

describe("DBSubnetGroupsListPage", () => {
  it("renders a subnet group row", () => {
    renderWithClient(<DBSubnetGroupsListPage />, seed([GROUP]))
    expect(
      screen.getByRole("link", { name: "orders-subnets" }),
    ).toBeInTheDocument()
    expect(screen.getByText("vpc-1")).toBeInTheDocument()
    expect(screen.getByText("2")).toBeInTheDocument()
    expect(screen.getByText("Private subnets")).toBeInTheDocument()
  })

  it("explains the fallback when no group exists", () => {
    renderWithClient(<DBSubnetGroupsListPage />, seed([]))
    expect(screen.getByText(/No DB subnet groups found/)).toBeInTheDocument()
  })

  it("names the in-use refusal in the delete dialog", () => {
    renderWithClient(<DBSubnetGroupsListPage />, seed([GROUP]))
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    expect(
      screen.getByText(/refused while any DB instance still references it/),
    ).toBeInTheDocument()
  })

  it("navigates to the create page from the heading action", () => {
    renderWithClient(<DBSubnetGroupsListPage />, seed([]))
    fireEvent.click(screen.getByRole("button", { name: "Create Subnet Group" }))
    expect(routerState.navigate).toHaveBeenCalledWith({
      to: "/rds/create-db-subnet-group",
    })
  })
})
