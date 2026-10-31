import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL } from "@/lib/panel-search"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const send = vi.hoisted(() =>
  vi.fn(
    async (_command: { constructor: { name: string }; input: unknown }) => ({}),
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

import { GroupsPage } from "./groups-page"

const GROUPS = [
  {
    GroupName: "admins",
    GroupId: "AGPA1",
    Arn: "arn:aws:iam::000000000001:group/admins",
    Path: "/",
    CreateDate: new Date("2026-01-01T00:00:00Z"),
  },
  {
    GroupName: "devs",
    GroupId: "AGPA2",
    Arn: "arn:aws:iam::000000000001:group/devs",
    Path: "/",
    CreateDate: new Date("2026-01-01T00:00:00Z"),
  },
]

describe("GroupsPage", () => {
  it("deletes the selected groups, one call each", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <GroupsPage
        onSearchChange={vi.fn()}
        groups={GROUPS}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select admins" }))
    await user.click(screen.getByRole("checkbox", { name: "Select devs" }))
    await user.click(screen.getByRole("button", { name: "Delete" }))
    expect(screen.getByText("devs (AGPA2)")).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })
    expect(
      send.mock.calls.map(([command]) => [
        command.constructor.name,
        command.input,
      ]),
    ).toStrictEqual([
      ["DeleteGroupCommand", { GroupName: "admins" }],
      ["DeleteGroupCommand", { GroupName: "devs" }],
    ])
  })
})
