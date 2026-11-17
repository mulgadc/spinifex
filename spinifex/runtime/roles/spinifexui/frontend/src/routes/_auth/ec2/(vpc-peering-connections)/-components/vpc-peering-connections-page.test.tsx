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

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL } from "@/lib/panel-search"
import { callerIdentityQueryOptions } from "@/queries/sts"

import { VpcPeeringConnectionsPage } from "./vpc-peering-connections-page"

const ROWS = [
  {
    VpcPeeringConnectionId: "pcx-1",
    Status: { Code: "active" as const },
    Tags: [{ Key: "Name", Value: "shared" }],
  },
  {
    VpcPeeringConnectionId: "pcx-2",
    Status: { Code: "pending-acceptance" as const },
  },
]

describe("VpcPeeringConnectionsPage", () => {
  it("deletes each selected connection with its own call", async () => {
    const user = userEvent.setup()
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(callerIdentityQueryOptions.queryKey, {
      $metadata: {},
      Account: "000000000002",
    })
    renderWithClient(
      <VpcPeeringConnectionsPage
        connections={ROWS}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      queryClient,
    )

    await user.click(screen.getByRole("checkbox", { name: "Select pcx-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select pcx-2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("shared (pcx-1)")).toBeInTheDocument()
    expect(within(dialog).getByText("pcx-2")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })
    expect(
      sdk.send.mock.calls
        .filter(
          ([command]) =>
            command.constructor.name === "DeleteVpcPeeringConnectionCommand",
        )
        .map(([command]) => command.input),
    ).toStrictEqual([
      { VpcPeeringConnectionId: "pcx-1" },
      { VpcPeeringConnectionId: "pcx-2" },
    ])
  })
})
