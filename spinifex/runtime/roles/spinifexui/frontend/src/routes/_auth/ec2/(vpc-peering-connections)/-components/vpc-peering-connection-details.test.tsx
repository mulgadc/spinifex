import type {
  VpcPeeringConnection,
  VpcPeeringConnectionStateReasonCode,
} from "@aws-sdk/client-ec2"
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

import { callerIdentityQueryOptions } from "@/queries/sts"

import { VpcPeeringConnectionDetails } from "./vpc-peering-connection-details"

const REQUESTER = "000000000002"
const ACCEPTER = "000000000003"

function connection(
  code: VpcPeeringConnectionStateReasonCode,
): VpcPeeringConnection {
  return {
    VpcPeeringConnectionId: "pcx-1",
    Status: { Code: code },
    RequesterVpcInfo: {
      VpcId: "vpc-a",
      OwnerId: REQUESTER,
      CidrBlock: "10.0.0.0/16",
    },
    AccepterVpcInfo: {
      VpcId: "vpc-b",
      OwnerId: ACCEPTER,
      CidrBlock: "10.1.0.0/16",
    },
  }
}

function render(pcx: VpcPeeringConnection, account: string) {
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(callerIdentityQueryOptions.queryKey, {
    $metadata: {},
    Account: account,
  })
  renderWithClient(
    <VpcPeeringConnectionDetails
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      pcx={pcx}
      tab="overview"
    />,
    queryClient,
  )
}

function sent(name: string) {
  return sdk.send.mock.calls
    .filter(([command]) => command.constructor.name === name)
    .map(([command]) => command.input)
}

describe("VpcPeeringConnectionDetails", () => {
  it.each([
    ["Accept request", "AcceptVpcPeeringConnectionCommand"],
    ["Reject request", "RejectVpcPeeringConnectionCommand"],
  ])(
    "lets the accepter's owner %s a pending request",
    async (button, command) => {
      const user = userEvent.setup()
      render(connection("pending-acceptance"), ACCEPTER)

      await user.click(screen.getByRole("button", { name: button }))

      await waitFor(() => {
        expect(sent(command)).toStrictEqual([
          { VpcPeeringConnectionId: "pcx-1" },
        ])
      })
    },
  )

  it.each([
    ["the requester, on a pending request", "pending-acceptance", REQUESTER],
    ["the accepter, on an active connection", "active", ACCEPTER],
  ] as const)("offers no answer to %s", (_, code, account) => {
    render(connection(code), account)

    expect(
      screen.queryByRole("button", { name: "Accept request" }),
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole("button", { name: "Reject request" }),
    ).not.toBeInTheDocument()
  })
})
