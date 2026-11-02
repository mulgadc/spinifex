import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import type { PanelTab } from "@/lib/panel-search"
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

import { DBSubnetGroupDetails } from "./db-subnet-group-details"

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

function renderDetails(tab: PanelTab) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "tags", "arn:orders-subnets"], { TagList: [] })
  renderWithClient(
    <DBSubnetGroupDetails
      group={GROUP}
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      tab={tab}
    />,
    qc,
  )
}

describe("DBSubnetGroupDetails", () => {
  it("lists each subnet with its availability zone", () => {
    renderDetails("overview")
    expect(screen.getAllByText("ap-southeast-2a")).toHaveLength(2)
    expect(screen.getByRole("link", { name: "vpc-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-vpcs/vpc-1",
    )
  })

  // The group owns membership; the subnet's own CIDR and addressing live on
  // the EC2 page, so each subnet links there rather than dead-ending here.
  it("links each subnet to its EC2 subnet page", () => {
    renderDetails("overview")
    expect(screen.getByRole("link", { name: "subnet-a1" })).toHaveAttribute(
      "href",
      "/ec2/describe-subnets/subnet-a1",
    )
    expect(screen.getByRole("link", { name: "subnet-a2" })).toHaveAttribute(
      "href",
      "/ec2/describe-subnets/subnet-a2",
    )
  })

  // The console has no edit path, so the CLI route needs stating rather than
  // leaving the user looking for one.
  it("points membership changes at the CLI", () => {
    renderDetails("overview")
    expect(screen.getByText(/modify-db-subnet-group/)).toBeInTheDocument()
  })

  it("names the in-use refusal in the delete dialog", () => {
    renderDetails("settings")
    fireEvent.click(screen.getByRole("button", { name: /Delete subnet group/ }))
    expect(
      screen.getByText(/refused while any DB instance still references it/),
    ).toBeInTheDocument()
  })
})
