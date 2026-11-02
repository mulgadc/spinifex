import type { NatGateway } from "@aws-sdk/client-ec2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk, navigate } = vi.hoisted(() => {
  interface SdkCommand {
    readonly input: object
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({})),
    },
    navigate: vi.fn(),
  }
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, {
    navigate,
    params: { id: "nat-1" },
  })
})

import { NatGatewayDetails } from "../-components/nat-gateway-details"
import { Route } from "./$id"

const NAT: NatGateway = {
  NatGatewayId: "nat-1",
  State: "available",
  SubnetId: "subnet-1",
  VpcId: "vpc-1",
  NatGatewayAddresses: [
    {
      AllocationId: "eipalloc-1",
      PublicIp: "203.0.113.5",
      PrivateIp: "10.0.1.9",
    },
  ],
}

async function renderPage() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "natGateways", "nat-1"], { NatGateways: [NAT] })
  const Detail = Route.options.component

  if (!Detail) {
    throw new Error("route has no component")
  }

  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, qc)
}

describe("NAT gateway detail", () => {
  it("links the Elastic IP behind the gateway", async () => {
    await renderPage()

    expect(screen.getByRole("link", { name: "eipalloc-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-addresses/eipalloc-1",
    )
    expect(screen.getByText("203.0.113.5")).toBeInTheDocument()
  })

  it("deletes only after it is confirmed, then closes", async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    renderWithClient(
      <NatGatewayDetails
        layout="page"
        nat={NAT}
        onClose={onClose}
        onTabChange={vi.fn()}
        tab="settings"
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("button", { name: "Delete NAT gateway" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { NatGatewayId: "nat-1" },
    })
  })

  it("offers no delete for a gateway already deleting", () => {
    renderWithClient(
      <NatGatewayDetails
        layout="page"
        nat={{ ...NAT, State: "deleting" }}
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        tab="settings"
      />,
      createTestQueryClient(),
    )

    expect(
      screen.getByRole("button", { name: "Delete NAT gateway" }),
    ).toBeDisabled()
  })
})
