import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn(async () => ({})))
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL } from "@/lib/panel-search"

import { RouteTableDetails } from "./route-table-details"
import { RouteTablesPage } from "./route-tables-page"

const MAIN = {
  RouteTableId: "rtb-main",
  VpcId: "vpc-1",
  Associations: [{ RouteTableAssociationId: "rtbassoc-m", Main: true }],
  Routes: [{ DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" }],
}

const CUSTOM = {
  RouteTableId: "rtb-2",
  VpcId: "vpc-1",
  Tags: [{ Key: "Name", Value: "public" }],
  Associations: [
    { RouteTableAssociationId: "rtbassoc-1", SubnetId: "subnet-1" },
    { RouteTableAssociationId: "rtbassoc-2", SubnetId: "subnet-2" },
  ],
}

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "internetGateways"], { InternetGateways: [] })
  qc.setQueryData(["ec2", "natGateways"], { NatGateways: [] })
  qc.setQueryData(["ec2", "subnets"], { Subnets: [] })

  return qc
}

describe("RouteTablesPage", () => {
  it("opens the panel for a clicked row", async () => {
    const user = userEvent.setup()
    const onSearchChange = vi.fn()
    renderWithClient(
      <RouteTablesPage
        onSearchChange={onSearchChange}
        routeTables={[MAIN, CUSTOM]}
        search={CLOSED_PANEL}
      />,
      seed(),
    )

    await user.click(screen.getByRole("cell", { name: "2" }))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "rtb-2",
    })
  })

  it("shows the selected table's routes and associations", () => {
    renderWithClient(
      <RouteTablesPage
        onSearchChange={vi.fn()}
        routeTables={[MAIN, CUSTOM]}
        search={{ ...CLOSED_PANEL, selected: "rtb-2" }}
      />,
      seed(),
    )

    expect(
      screen.getByRole("region", { name: "Route table details" }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Disassociate subnet-2" }),
    ).toBeInTheDocument()
  })
})

describe("RouteTableDetails", () => {
  it("keeps a main route table's delete visible but disabled", () => {
    renderWithClient(
      <RouteTableDetails
        layout="page"
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        rtb={MAIN}
        tab="settings"
      />,
      seed(),
    )

    expect(
      screen.getByRole("button", { name: "Delete route table" }),
    ).toBeDisabled()
  })
})
