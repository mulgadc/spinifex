import { screen, waitFor, within } from "@testing-library/react"
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

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL } from "@/lib/panel-search"

import { NatGatewaysPage } from "./nat-gateways-page"

const ROWS = [
  {
    NatGatewayId: "nat-1",
    State: "available" as const,
    Tags: [{ Key: "Name", Value: "egress" }],
  },
  { NatGatewayId: "nat-2", State: "available" as const },
]

describe("NatGatewaysPage", () => {
  it("deletes each selected NAT gateway with its own call", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <NatGatewaysPage
        natGateways={ROWS}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select nat-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select nat-2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("egress (nat-1)")).toBeInTheDocument()
    expect(within(dialog).getByText("nat-2")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })
    expect(
      sdk.send.mock.calls
        .filter(
          ([command]) => command.constructor.name === "DeleteNatGatewayCommand",
        )
        .map(([command]) => command.input),
    ).toStrictEqual([{ NatGatewayId: "nat-1" }, { NatGatewayId: "nat-2" }])
  })
})
