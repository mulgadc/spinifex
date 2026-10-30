import type { Snapshot } from "@aws-sdk/client-ec2"
import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const send = vi.hoisted(() =>
  vi.fn(
    async (_command: { constructor: { name: string }; input: object }) => ({}),
  ),
)

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_SNAPSHOTS_PANEL, SnapshotsPage } from "./snapshots-page"

const ROWS: Snapshot[] = [
  { SnapshotId: "snap-1", State: "completed" },
  { SnapshotId: "snap-2", State: "completed" },
]

describe("SnapshotsPage", () => {
  it("deletes each selected row with its own call", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <SnapshotsPage
        snapshots={ROWS}
        onSearchChange={vi.fn()}
        search={CLOSED_SNAPSHOTS_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select snap-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select snap-2" }))

    const bar = screen.getByRole("toolbar", { name: "Selection actions" })

    expect(bar).toHaveTextContent("2 selected")
    await user.click(within(bar).getByRole("button", { name: "Delete" }))

    const dialog = screen.getByRole("alertdialog")

    expect(
      within(dialog).getByRole("list", { name: "Selected snapshots" }),
    ).toHaveTextContent("snap-2")
    await user.type(
      await screen.findByLabelText(/^To confirm, type/u),
      "delete",
    )
    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    expect(
      send.mock.calls.map(([command]) => [
        command.constructor.name,
        command.input,
      ]),
    ).toStrictEqual([
      ["DeleteSnapshotCommand", { SnapshotId: "snap-1" }],
      ["DeleteSnapshotCommand", { SnapshotId: "snap-2" }],
    ])
  })
})
