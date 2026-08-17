import { screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => vi.fn(),
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { DBSubnetGroupDetailPage } from "./db-subnet-group-detail-page"

const GROUP = {
  DBSubnetGroupName: "orders-subnets",
  DBSubnetGroupDescription: "Private subnets",
  DBSubnetGroupArn: "arn:orders-subnets",
  SubnetGroupStatus: "Complete",
  VpcId: "vpc-1",
  Subnets: [
    {
      SubnetIdentifier: "subnet-a1",
      SubnetStatus: "Active",
      SubnetAvailabilityZone: { Name: "ap-southeast-2a" },
    },
    {
      SubnetIdentifier: "subnet-a2",
      SubnetStatus: "Active",
      SubnetAvailabilityZone: { Name: "ap-southeast-2a" },
    },
  ],
}

function seed(group: unknown) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "subnetGroups", "orders-subnets"], {
    DBSubnetGroups: group ? [group] : [],
  })
  qc.setQueryData(["rds", "tags", "arn:orders-subnets"], { TagList: [] })
  qc.setQueryData(["rds", "tags", ""], { TagList: [] })
  return qc
}

describe("DBSubnetGroupDetailPage", () => {
  it("lists each subnet with its availability zone", () => {
    renderWithClient(
      <DBSubnetGroupDetailPage dbSubnetGroupName="orders-subnets" />,
      seed(GROUP),
    )
    expect(screen.getByText("subnet-a1")).toBeInTheDocument()
    expect(screen.getByText("subnet-a2")).toBeInTheDocument()
    expect(screen.getAllByText("ap-southeast-2a")).toHaveLength(2)
    expect(screen.getByText("vpc-1")).toBeInTheDocument()
  })

  // ModifyDBSubnetGroup is not implemented, so the absence of an edit path
  // needs stating rather than leaving the user looking for one.
  it("explains that membership is fixed at create", () => {
    renderWithClient(
      <DBSubnetGroupDetailPage dbSubnetGroupName="orders-subnets" />,
      seed(GROUP),
    )
    expect(
      screen.getByText(/ModifyDBSubnetGroup is not implemented/),
    ).toBeInTheDocument()
  })

  it("reports a group that does not exist", () => {
    renderWithClient(
      <DBSubnetGroupDetailPage dbSubnetGroupName="orders-subnets" />,
      seed(null),
    )
    expect(screen.getByText("DB subnet group not found.")).toBeInTheDocument()
  })

  it("offers the delete action", () => {
    renderWithClient(
      <DBSubnetGroupDetailPage dbSubnetGroupName="orders-subnets" />,
      seed(GROUP),
    )
    expect(screen.getByRole("button", { name: /Delete/ })).toBeEnabled()
  })
})
