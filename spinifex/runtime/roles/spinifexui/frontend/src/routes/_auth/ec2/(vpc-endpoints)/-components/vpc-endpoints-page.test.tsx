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
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({
        Unsuccessful: [
          {
            ResourceId: "vpce-2",
            Error: {
              Code: "InvalidVpcEndpoint.NotFound",
              Message: "The VPC endpoint vpce-2 does not exist",
            },
          },
        ],
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

import { CLOSED_PANEL } from "@/lib/panel-search"

import { VpcEndpointsPage } from "./vpc-endpoints-page"

const ROWS = [
  {
    VpcEndpointId: "vpce-1",
    State: "Available" as const,
    Tags: [{ Key: "Name", Value: "s3" }],
  },
  { VpcEndpointId: "vpce-2", State: "Available" as const },
]

describe("VpcEndpointsPage", () => {
  it("deletes the selection in one call and keeps each refusal against its endpoint", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <VpcEndpointsPage
        endpoints={ROWS}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select vpce-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select vpce-2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("s3 (vpce-1)")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await expect(
      within(dialog).findByText("The VPC endpoint vpce-2 does not exist"),
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
            command.constructor.name === "DeleteVpcEndpointsCommand",
        )
        .map(([command]) => command.input),
    ).toStrictEqual([{ VpcEndpointIds: ["vpce-1", "vpce-2"] }])
  })
})
