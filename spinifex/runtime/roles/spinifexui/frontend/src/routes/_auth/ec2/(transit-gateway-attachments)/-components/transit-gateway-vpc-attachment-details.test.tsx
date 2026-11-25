import type { TransitGatewayVpcAttachment } from "@aws-sdk/client-ec2"
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

  return await mockRouter(importOriginal)
})

import { TransitGatewayVpcAttachmentDetails } from "./transit-gateway-vpc-attachment-details"

const ATTACHMENT: TransitGatewayVpcAttachment = {
  TransitGatewayAttachmentId: "tgw-attach-1",
  TransitGatewayId: "tgw-1",
  VpcId: "vpc-1",
  State: "available",
  SubnetIds: ["subnet-1"],
}

function renderDetails(
  attachment: TransitGatewayVpcAttachment,
  tab: "overview" | "settings",
) {
  const onClose = vi.fn()
  renderWithClient(
    <TransitGatewayVpcAttachmentDetails
      attachment={attachment}
      layout="page"
      onClose={onClose}
      onTabChange={vi.fn()}
      tab={tab}
    />,
    createTestQueryClient(),
  )

  return onClose
}

describe("transit gateway attachment details", () => {
  it("links the gateway, the VPC and the subnets", () => {
    renderDetails(ATTACHMENT, "overview")

    expect(screen.getByRole("link", { name: "tgw-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-transit-gateways/tgw-1",
    )
    expect(screen.getByRole("link", { name: "vpc-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-vpcs/vpc-1",
    )
    expect(screen.getByRole("link", { name: "subnet-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-subnets/subnet-1",
    )
  })

  it("deletes only after it is confirmed, then closes", async () => {
    const user = userEvent.setup()
    const onClose = renderDetails(ATTACHMENT, "settings")

    await user.click(screen.getByRole("button", { name: "Delete attachment" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { TransitGatewayAttachmentId: "tgw-attach-1" },
    })
  })

  it("offers no delete for an attachment already deleted", () => {
    renderDetails({ ...ATTACHMENT, State: "deleted" }, "settings")

    expect(
      screen.getByRole("button", { name: "Delete attachment" }),
    ).toBeDisabled()
  })
})
