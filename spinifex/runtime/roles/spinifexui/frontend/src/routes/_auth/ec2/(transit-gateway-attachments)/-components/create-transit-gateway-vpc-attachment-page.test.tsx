import type { TransitGateway } from "@aws-sdk/client-ec2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly constructor: { name: string }
    readonly input: object
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({
        TransitGatewayVpcAttachment: {
          TransitGatewayAttachmentId: "tgw-attach-new",
        },
      })),
    },
  }
})

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import {
  ec2SubnetsQueryOptions,
  ec2TransitGatewaysQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"

import { CreateTransitGatewayVpcAttachmentPage } from "./create-transit-gateway-vpc-attachment-page"

const GATEWAYS: TransitGateway[] = [
  { TransitGatewayId: "tgw-ready", State: "available" },
  { TransitGatewayId: "tgw-new", State: "pending" },
]

function setup(gateways: TransitGateway[]) {
  const onClose = vi.fn()
  const qc = createTestQueryClient()
  qc.setQueryData(ec2TransitGatewaysQueryOptions.queryKey, {
    $metadata: {},
    TransitGateways: gateways,
  })
  qc.setQueryData(ec2VpcsQueryOptions.queryKey, {
    $metadata: {},
    Vpcs: [
      { VpcId: "vpc-a", CidrBlock: "10.0.0.0/16" },
      { VpcId: "vpc-b", CidrBlock: "10.1.0.0/16" },
    ],
  })
  qc.setQueryData(ec2SubnetsQueryOptions.queryKey, {
    $metadata: {},
    Subnets: [
      { SubnetId: "subnet-a1", VpcId: "vpc-a", AvailabilityZone: "az-1" },
      { SubnetId: "subnet-a2", VpcId: "vpc-a", AvailabilityZone: "az-1" },
      { SubnetId: "subnet-a3", VpcId: "vpc-a", AvailabilityZone: "az-2" },
      { SubnetId: "subnet-b1", VpcId: "vpc-b", AvailabilityZone: "az-1" },
    ],
  })
  renderWithClient(
    <CreateTransitGatewayVpcAttachmentPage onClose={onClose} />,
    qc,
  )

  return onClose
}

function created() {
  return sdk.send.mock.calls
    .filter(
      ([command]) =>
        command.constructor.name === "CreateTransitGatewayVpcAttachmentCommand",
    )
    .map(([command]) => command.input)
}

async function pick(combobox: string, option: string) {
  const user = userEvent.setup()

  await user.click(screen.getByRole("combobox", { name: combobox }))
  await user.click(await screen.findByRole("option", { name: option }))
}

describe("CreateTransitGatewayVpcAttachmentPage", () => {
  it("offers only available gateways", async () => {
    const user = userEvent.setup()
    setup(GATEWAYS)

    await user.click(screen.getByRole("combobox", { name: "Transit gateway" }))

    const options = await screen.findAllByRole("option")

    expect(options.map((o) => o.textContent)).toStrictEqual(["tgw-ready"])
  })

  it("attaches the VPC through one chosen subnet in each zone", async () => {
    const user = userEvent.setup()
    const onClose = setup(GATEWAYS)

    await pick("Transit gateway", "tgw-ready")
    await pick("VPC", "vpc-a (10.0.0.0/16)")

    expect(
      screen.queryByRole("switch", { name: "Subnet subnet-b1" }),
    ).not.toBeInTheDocument()

    await user.click(screen.getByRole("switch", { name: "Subnet subnet-a1" }))
    await user.click(screen.getByRole("switch", { name: "Subnet subnet-a3" }))
    await user.click(screen.getByRole("switch", { name: "Subnet subnet-a2" }))

    expect(
      screen.getByRole("switch", { name: "Subnet subnet-a1" }),
    ).not.toBeChecked()

    await user.click(
      screen.getByRole("button", { name: "Create VPC attachment" }),
    )

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("tgw-attach-new")
    })
    expect(created()).toStrictEqual([
      {
        TransitGatewayId: "tgw-ready",
        VpcId: "vpc-a",
        SubnetIds: ["subnet-a3", "subnet-a2"],
        TagSpecifications: undefined,
      },
    ])
  })

  it("refuses to send without a subnet", async () => {
    const user = userEvent.setup()
    setup(GATEWAYS)

    await pick("Transit gateway", "tgw-ready")
    await pick("VPC", "vpc-a (10.0.0.0/16)")
    await user.click(
      screen.getByRole("button", { name: "Create VPC attachment" }),
    )

    await expect(
      screen.findByText("Choose at least one subnet"),
    ).resolves.toBeInTheDocument()
    expect(created()).toStrictEqual([])
  })

  it("says no gateway is available and cannot be sent", () => {
    setup([{ TransitGatewayId: "tgw-new", State: "pending" }])

    expect(
      screen.getByText("No transit gateway is available. Create one first."),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create VPC attachment" }),
    ).toBeDisabled()
  })
})
