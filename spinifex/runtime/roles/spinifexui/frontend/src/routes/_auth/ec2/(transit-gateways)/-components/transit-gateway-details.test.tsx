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

import { ec2GatewayTransitGatewayAttachmentsQueryOptions } from "@/queries/ec2"

import { TransitGatewayDetails } from "./transit-gateway-details"

const TGW: TransitGateway = {
  TransitGatewayId: "tgw-1",
  State: "available",
  Description: "hub",
  Options: { AmazonSideAsn: 64_512, DnsSupport: "enable" },
}

function renderDetails(tgw: TransitGateway, tab: "overview" | "settings") {
  const qc = createTestQueryClient()
  qc.setQueryData(
    ec2GatewayTransitGatewayAttachmentsQueryOptions("tgw-1").queryKey,
    {
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
      ],
    },
  )
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
  it("lists the gateway's attachments that are not deleted", () => {
    renderDetails(TGW, "overview")

    expect(
      screen.getByRole("link", { name: "tgw-attach-live" }),
    ).toHaveAttribute(
      "href",
      "/ec2/describe-transit-gateway-vpc-attachments/tgw-attach-live",
    )
    expect(
      screen.queryByRole("link", { name: "tgw-attach-gone" }),
    ).not.toBeInTheDocument()
  })

  it("says the attachments failed to load rather than that there are none", async () => {
    sdk.send.mockRejectedValueOnce(new Error("UnauthorizedOperation"))
    renderWithClient(
      <TransitGatewayDetails
        layout="page"
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        tab="overview"
        tgw={TGW}
      />,
      createTestQueryClient(),
    )

    await expect(
      screen.findByText("Failed to load VPC attachments"),
    ).resolves.toBeInTheDocument()
    expect(screen.queryByText(/No VPC attachments/)).not.toBeInTheDocument()
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
