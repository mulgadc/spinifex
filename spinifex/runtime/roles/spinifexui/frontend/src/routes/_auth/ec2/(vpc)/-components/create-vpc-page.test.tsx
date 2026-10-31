import type {
  CreateSubnetCommandInput,
  CreateVpcCommandInput,
} from "@aws-sdk/client-ec2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

import { CreateVpcPage } from "./create-vpc-page"

const onClose = vi.fn()

function renderPage() {
  renderWithClient(<CreateVpcPage onClose={onClose} />, createTestQueryClient())
}

describe("CreateVpcPage", () => {
  beforeEach(() => {
    sdk.reset()
    onClose.mockReset()
    sdk.setHandler("DescribeVpcsCommand", () => ({ Vpcs: [] }))
    sdk.setHandler("DescribeSubnetsCommand", () => ({ Subnets: [] }))
  })

  it("names the VPC with the bare prefix when auto-generate is off", async () => {
    const user = userEvent.setup()

    const createVpc = vi.fn((_input: CreateVpcCommandInput) => ({
      Vpc: { VpcId: "vpc-1" },
    }))

    sdk.setHandler("CreateVpcCommand", createVpc)
    renderPage()

    await user.type(screen.getByLabelText("Name tag auto-generation"), "proj")
    await user.click(screen.getByLabelText("Auto-generate name tags"))
    await user.click(screen.getByRole("button", { name: "Create VPC" }))

    await waitFor(() => {
      expect(createVpc).toHaveBeenCalled()
    })
    expect(
      createVpc.mock.calls[0]?.[0].TagSpecifications?.[0]?.Tags,
    ).toStrictEqual([{ Key: "Name", Value: "proj" }])
  })

  it("shows the failed step and clears it when the wizard is resubmitted", async () => {
    const user = userEvent.setup()
    let failSubnet = true
    sdk.setHandler("CreateVpcCommand", () => ({ Vpc: { VpcId: "vpc-1" } }))
    sdk.setHandler(
      "CreateSubnetCommand",
      (_input: CreateSubnetCommandInput) => {
        if (failSubnet) {
          throw new Error("subnet quota exceeded")
        }

        return { Subnet: { SubnetId: "subnet-1" } }
      },
    )
    sdk.setHandler("ModifySubnetAttributeCommand", () => ({}))
    sdk.setHandler("CreateInternetGatewayCommand", () => ({
      InternetGateway: { InternetGatewayId: "igw-1" },
    }))
    sdk.setHandler("AttachInternetGatewayCommand", () => ({}))
    sdk.setHandler("CreateRouteTableCommand", () => ({
      RouteTable: { RouteTableId: "rtb-1" },
    }))
    sdk.setHandler("CreateRouteCommand", () => ({}))
    sdk.setHandler("AssociateRouteTableCommand", () => ({}))
    renderPage()

    await user.click(screen.getByLabelText("VPC and more"))
    expect(screen.getByText("Number of public subnets")).toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: "Create VPC" }))

    await screen.findByText(
      "Wizard failed: Failed while creating public subnets",
    )
    expect(onClose).not.toHaveBeenCalled()

    failSubnet = false
    await user.click(screen.getByRole("button", { name: "Create VPC" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("vpc-1")
    })
    expect(screen.queryByText(/Wizard failed/)).not.toBeInTheDocument()
  })

  it("shows a refused VPC-only create beside the button and stays open", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateVpcCommand", () => {
      throw Object.assign(
        new Error("The maximum number of VPCs has been reached."),
        {
          name: "VpcLimitExceeded",
        },
      )
    })
    renderPage()

    await user.click(screen.getByRole("button", { name: "Create VPC" }))

    await expect(
      screen.findByText(
        "VpcLimitExceeded: The maximum number of VPCs has been reached.",
      ),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByRole("button", { name: "Create VPC" })).toBeEnabled()
  })
})
