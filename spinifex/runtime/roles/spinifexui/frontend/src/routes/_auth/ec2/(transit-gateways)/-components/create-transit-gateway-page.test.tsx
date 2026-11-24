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
        TransitGateway: { TransitGatewayId: "tgw-new" },
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

import { CreateTransitGatewayPage } from "./create-transit-gateway-page"

function created() {
  return sdk.send.mock.calls
    .filter(
      ([command]) => command.constructor.name === "CreateTransitGatewayCommand",
    )
    .map(([command]) => command.input)
}

describe("CreateTransitGatewayPage", () => {
  it("creates a gateway with its name and description, and selects it", async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    renderWithClient(
      <CreateTransitGatewayPage onClose={onClose} />,
      createTestQueryClient(),
    )

    await user.type(screen.getByLabelText("Name (optional)"), "hub")
    await user.type(screen.getByLabelText("Description (optional)"), " core ")
    await user.click(
      screen.getByRole("button", { name: "Create transit gateway" }),
    )

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("tgw-new")
    })
    expect(created()).toStrictEqual([
      {
        Description: "core",
        TagSpecifications: [
          {
            ResourceType: "transit-gateway",
            Tags: [{ Key: "Name", Value: "hub" }],
          },
        ],
      },
    ])
  })

  it("sends no description or tags when both are left empty", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <CreateTransitGatewayPage onClose={vi.fn()} />,
      createTestQueryClient(),
    )

    await user.click(
      screen.getByRole("button", { name: "Create transit gateway" }),
    )

    await waitFor(() => {
      expect(created()).toStrictEqual([
        { Description: undefined, TagSpecifications: undefined },
      ])
    })
  })
})
