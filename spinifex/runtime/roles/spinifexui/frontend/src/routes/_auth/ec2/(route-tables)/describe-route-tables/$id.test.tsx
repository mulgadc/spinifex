import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly input: object
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({})),
    },
  }
})

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { params: { id: "rtb-1" } })
})

import { Route } from "./$id"

function peering(
  id: string,
  code: string,
  requester: string,
  accepter: string,
) {
  return {
    VpcPeeringConnectionId: id,
    Status: { Code: code },
    RequesterVpcInfo: { VpcId: requester, CidrBlock: "10.0.0.0/16" },
    AccepterVpcInfo: { VpcId: accepter, CidrBlock: "10.1.0.0/16" },
  }
}

async function renderPage() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "routeTables", "rtb-1"], {
    RouteTables: [
      {
        RouteTableId: "rtb-1",
        VpcId: "vpc-1",
        Routes: [
          { DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" },
          { DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1" },
        ],
        Associations: [
          { RouteTableAssociationId: "rtbassoc-1", SubnetId: "subnet-1" },
        ],
      },
    ],
  })
  qc.setQueryData(["ec2", "internetGateways"], { InternetGateways: [] })
  qc.setQueryData(["ec2", "natGateways"], { NatGateways: [] })
  qc.setQueryData(["ec2", "vpcPeeringConnections"], {
    VpcPeeringConnections: [
      peering("pcx-pending", "pending-acceptance", "vpc-1", "vpc-2"),
      peering("pcx-accepted", "active", "vpc-3", "vpc-1"),
      peering("pcx-deleted", "deleted", "vpc-1", "vpc-2"),
      peering("pcx-elsewhere", "active", "vpc-2", "vpc-3"),
    ],
  })
  qc.setQueryData(["ec2", "transitGateways"], {
    TransitGateways: [
      { TransitGatewayId: "tgw-ready", State: "available" },
      {
        TransitGatewayId: "tgw-named",
        State: "available",
        Tags: [{ Key: "Name", Value: "core" }],
      },
      { TransitGatewayId: "tgw-elsewhere", State: "available" },
    ],
  })
  qc.setQueryData(["ec2", "transitGatewayVpcAttachments"], {
    TransitGatewayVpcAttachments: [
      { TransitGatewayId: "tgw-ready", VpcId: "vpc-1", State: "available" },
      { TransitGatewayId: "tgw-named", VpcId: "vpc-1", State: "pending" },
      { TransitGatewayId: "tgw-elsewhere", VpcId: "vpc-2", State: "available" },
    ],
  })
  qc.setQueryData(["ec2", "subnets"], { Subnets: [] })
  const Detail = Route.options.component

  if (!Detail) {
    throw new Error("route has no component")
  }

  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, qc)
}

describe("route table detail", () => {
  it("deletes a route only after it is confirmed", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole("button", { name: "Delete route to 0.0.0.0/0" }),
    )
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { RouteTableId: "rtb-1", DestinationCidrBlock: "0.0.0.0/0" },
    })
  })

  it("keeps the route when the dialog is cancelled", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole("button", { name: "Delete route to 0.0.0.0/0" }),
    )
    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(sdk.send).not.toHaveBeenCalled()
  })

  it("disassociates a subnet only after it is confirmed", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole("button", { name: "Disassociate subnet-1" }),
    )
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Disassociate" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { AssociationId: "rtbassoc-1" },
    })
  })

  it("routes to a live peering connection of the table's own VPC", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole("combobox", { name: "Target type" }))
    await user.click(
      await screen.findByRole("option", { name: "Peering connection" }),
    )
    await user.click(screen.getByRole("combobox", { name: "Target" }))

    const options = await screen.findAllByRole("option")

    expect(options.map((o) => o.textContent)).toStrictEqual([
      "pcx-pending (vpc-2 10.1.0.0/16)",
      "pcx-accepted (vpc-3 10.0.0.0/16)",
    ])

    await user.click(
      screen.getByRole("option", { name: "pcx-accepted (vpc-3 10.0.0.0/16)" }),
    )
    await user.clear(screen.getByLabelText("Destination"))
    await user.type(screen.getByLabelText("Destination"), "10.3.0.0/16")
    await user.click(screen.getByRole("button", { name: "Add route" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: {
        RouteTableId: "rtb-1",
        DestinationCidrBlock: "10.3.0.0/16",
        VpcPeeringConnectionId: "pcx-accepted",
      },
    })
  })

  it("routes to a transit gateway attached to the table's own VPC", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole("combobox", { name: "Target type" }))
    await user.click(
      await screen.findByRole("option", { name: "Transit gateway" }),
    )
    await user.click(screen.getByRole("combobox", { name: "Target" }))

    const options = await screen.findAllByRole("option")

    expect(options.map((o) => o.textContent)).toStrictEqual([
      "tgw-ready",
      "tgw-named (core)",
    ])

    await user.click(screen.getByRole("option", { name: "tgw-named (core)" }))
    await user.clear(screen.getByLabelText("Destination"))
    await user.type(screen.getByLabelText("Destination"), "10.8.0.0/16")
    await user.click(screen.getByRole("button", { name: "Add route" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: {
        RouteTableId: "rtb-1",
        DestinationCidrBlock: "10.8.0.0/16",
        TransitGatewayId: "tgw-named",
      },
    })
  })
})
