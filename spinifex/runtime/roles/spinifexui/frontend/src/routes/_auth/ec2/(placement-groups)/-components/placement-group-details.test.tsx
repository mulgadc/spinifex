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

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { PlacementGroupDetails } from "./placement-group-details"

describe("placement group details", () => {
  it("deletes by group name only after it is confirmed, then closes", async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    const qc = createTestQueryClient()
    qc.setQueryData(["ec2", "instances"], { Reservations: [] })
    renderWithClient(
      <PlacementGroupDetails
        layout="page"
        onClose={onClose}
        onTabChange={vi.fn()}
        pg={{ GroupId: "pg-1", GroupName: "web", State: "available" }}
        tab="settings"
      />,
      qc,
    )

    await user.click(
      screen.getByRole("button", { name: "Delete placement group" }),
    )
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { GroupName: "web" },
    })
  })
})
