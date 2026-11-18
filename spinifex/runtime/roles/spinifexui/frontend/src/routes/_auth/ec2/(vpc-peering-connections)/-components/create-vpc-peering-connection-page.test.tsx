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
        VpcPeeringConnection: { VpcPeeringConnectionId: "pcx-new" },
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

import { ec2VpcsQueryOptions } from "@/queries/ec2"

import { CreateVpcPeeringConnectionPage } from "./create-vpc-peering-connection-page"

function setup() {
  const onClose = vi.fn()
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(ec2VpcsQueryOptions.queryKey, {
    $metadata: {},
    Vpcs: [
      { VpcId: "vpc-a", CidrBlock: "10.0.0.0/16" },
      { VpcId: "vpc-b", CidrBlock: "10.1.0.0/16" },
    ],
  })
  renderWithClient(
    <CreateVpcPeeringConnectionPage onClose={onClose} />,
    queryClient,
  )

  return onClose
}

function created() {
  return sdk.send.mock.calls
    .filter(
      ([command]) =>
        command.constructor.name === "CreateVpcPeeringConnectionCommand",
    )
    .map(([command]) => command.input)
}

async function pick(label: string, option: string) {
  const user = userEvent.setup()

  await user.click(screen.getByRole("combobox", { name: label }))
  await user.click(await screen.findByRole("option", { name: option }))
}

describe("CreateVpcPeeringConnectionPage", () => {
  it("peers two of the caller's VPCs, offering each only once", async () => {
    const user = userEvent.setup()
    const onClose = setup()

    await pick("Requester VPC", "vpc-a (10.0.0.0/16)")
    await user.click(screen.getByRole("combobox", { name: "Accepter VPC" }))

    const options = await screen.findAllByRole("option")

    expect(options.map((o) => o.textContent)).toStrictEqual([
      "vpc-b (10.1.0.0/16)",
    ])

    await user.click(
      screen.getByRole("option", { name: "vpc-b (10.1.0.0/16)" }),
    )
    await user.click(
      screen.getByRole("button", { name: "Create peering connection" }),
    )

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("pcx-new")
    })
    expect(created()).toStrictEqual([
      {
        VpcId: "vpc-a",
        PeerVpcId: "vpc-b",
        PeerOwnerId: undefined,
        TagSpecifications: undefined,
      },
    ])
  })

  it("sends the peer account for a VPC in another account", async () => {
    const user = userEvent.setup()
    const onClose = setup()

    await pick("Requester VPC", "vpc-a (10.0.0.0/16)")
    await user.click(screen.getByRole("radio", { name: "Another account" }))
    await user.type(screen.getByLabelText("Account ID"), "000000000003")
    await user.type(screen.getByLabelText("Accepter VPC"), "vpc-remote")
    await user.type(screen.getByLabelText("Name (optional)"), "to-remote")
    await user.click(
      screen.getByRole("button", { name: "Create peering connection" }),
    )

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("pcx-new")
    })
    expect(created()).toStrictEqual([
      {
        VpcId: "vpc-a",
        PeerVpcId: "vpc-remote",
        PeerOwnerId: "000000000003",
        TagSpecifications: [
          {
            ResourceType: "vpc-peering-connection",
            Tags: [{ Key: "Name", Value: "to-remote" }],
          },
        ],
      },
    ])
  })

  it("refuses an account ID that is not 12 digits", async () => {
    const user = userEvent.setup()
    setup()

    await pick("Requester VPC", "vpc-a (10.0.0.0/16)")
    await user.click(screen.getByRole("radio", { name: "Another account" }))
    await user.type(screen.getByLabelText("Account ID"), "12345")
    await user.type(screen.getByLabelText("Accepter VPC"), "vpc-remote")
    await user.click(
      screen.getByRole("button", { name: "Create peering connection" }),
    )

    await expect(
      screen.findByText("Account ID must be 12 digits"),
    ).resolves.toBeInTheDocument()
    expect(created()).toStrictEqual([])
  })
})
