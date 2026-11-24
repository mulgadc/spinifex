import type { TransitGateway } from "@aws-sdk/client-ec2"
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

import { ec2TransitGatewayVpcAttachmentsQueryOptions } from "@/queries/ec2"

import { TransitGatewayDetails } from "./transit-gateway-details"

const TGW: TransitGateway = {
  TransitGatewayId: "tgw-1",
  State: "available",
  Description: "hub",
  Options: { AmazonSideAsn: 64_512, DnsSupport: "enable" },
}

function renderDetails(tgw: TransitGateway, tab: "overview" | "settings") {
  const qc = createTestQueryClient()
  qc.setQueryData(ec2TransitGatewayVpcAttachmentsQueryOptions.queryKey, {
    $metadata: {},
    TransitGatewayVpcAttachments: [
      {
        TransitGatewayAttachmentId: "tgw-attach-live",
        TransitGatewayId: "tgw-1",
        VpcId: "vpc-1",
        State: "available",
      },
      {
        TransitGatewayAttachmentId: "tgw-attach-gone",
        TransitGatewayId: "tgw-1",
        VpcId: "vpc-2",
        State: "deleted",
      },
      {
        TransitGatewayAttachmentId: "tgw-attach-other",
        TransitGatewayId: "tgw-2",
        VpcId: "vpc-3",
        State: "available",
      },
    ],
  })
  const onClose = vi.fn()
  renderWithClient(
    <TransitGatewayDetails
      layout="page"
      onClose={onClose}
      onTabChange={vi.fn()}
      tab={tab}
      tgw={tgw}
    />,
    qc,
  )

  return onClose
}

describe("transit gateway details", () => {
  it("lists only the gateway's own attachments that are not deleted", () => {
    renderDetails(TGW, "overview")

    expect(screen.getByText("VPC attachments (1)")).toBeInTheDocument()
    expect(
      screen.getByRole("link", { name: "tgw-attach-live" }),
    ).toHaveAttribute(
      "href",
      "/ec2/describe-transit-gateway-vpc-attachments/tgw-attach-live",
    )
    expect(
      screen.queryByRole("link", { name: "tgw-attach-gone" }),
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole("link", { name: "tgw-attach-other" }),
    ).not.toBeInTheDocument()
  })

  it("deletes only after it is confirmed, then closes", async () => {
    const user = userEvent.setup()
    const onClose = renderDetails(TGW, "settings")

    await user.click(
      screen.getByRole("button", { name: "Delete transit gateway" }),
    )
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { TransitGatewayId: "tgw-1" },
    })
  })

  it("offers no delete for a gateway already deleting", () => {
    renderDetails({ ...TGW, State: "deleting" }, "settings")

    expect(
      screen.getByRole("button", { name: "Delete transit gateway" }),
    ).toBeDisabled()
  })
})
