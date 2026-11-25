import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly constructor: { name: string }
    readonly input: { TransitGatewayId?: string }
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(
        async (command) => {
          if (command.input.TransitGatewayId === "tgw-2") {
            throw new Error(
              "tgw-2 has non-deleted Transit Gateway Attachments: tgw-attach-1.",
            )
          }

          return {}
        },
      ),
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

import { CLOSED_PANEL } from "@/lib/panel-search"

import { TransitGatewaysPage } from "./transit-gateways-page"

const ROWS = [
  {
    TransitGatewayId: "tgw-1",
    State: "available" as const,
    Tags: [{ Key: "Name", Value: "hub" }],
  },
  { TransitGatewayId: "tgw-2", State: "available" as const },
]

describe("TransitGatewaysPage", () => {
  it("deletes each selected gateway with its own call and keeps a refusal against its gateway", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <TransitGatewaysPage
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
        transitGateways={ROWS}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select tgw-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select tgw-2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("hub (tgw-1)")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await expect(
      within(dialog).findByText(/non-deleted Transit Gateway Attachments/),
    ).resolves.toBeInTheDocument()
    await waitFor(() => {
      expect(
        within(dialog).getByRole("button", { name: "Retry 1" }),
      ).toBeInTheDocument()
    })
    expect(
      sdk.send.mock.calls
        .filter(
          ([command]) =>
            command.constructor.name === "DeleteTransitGatewayCommand",
        )
        .map(([command]) => command.input),
    ).toStrictEqual([
      { TransitGatewayId: "tgw-1" },
      { TransitGatewayId: "tgw-2" },
    ])
  })
})
