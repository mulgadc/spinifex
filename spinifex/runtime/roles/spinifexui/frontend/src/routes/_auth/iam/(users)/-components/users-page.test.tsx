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

import { UsersPage } from "./users-page"

const USERS = ["alice", "bob"].map((userName, index) => ({
  UserName: userName,
  UserId: `AIDA${index + 1}`,
  Arn: `arn:aws:iam::000000000001:user/${userName}`,
  Path: "/",
  CreateDate: new Date("2026-01-01T00:00:00Z"),
}))

describe("UsersPage", () => {
  it("opens a user's panel from its row, keyed by name", async () => {
    const user = userEvent.setup()
    const onSearchChange = vi.fn()
    renderWithClient(
      <UsersPage
        onSearchChange={onSearchChange}
        search={CLOSED_PANEL}
        users={USERS}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("cell", { name: "AIDA2" }))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "bob",
    })
  })

  it("shows the selected user's details", () => {
    renderWithClient(
      <UsersPage
        onSearchChange={vi.fn()}
        search={{ ...CLOSED_PANEL, selected: "alice" }}
        users={USERS}
      />,
      createTestQueryClient(),
    )

    expect(
      screen.getByRole("region", { name: "User details" }),
    ).toHaveTextContent("AIDA1")
  })

  it("deletes the selected users, one call each", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <UsersPage
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
        users={USERS}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select alice" }))
    await user.click(screen.getByRole("checkbox", { name: "Select bob" }))
    await user.click(screen.getByRole("button", { name: "Delete" }))
    expect(screen.getByText("bob (AIDA2)")).toBeInTheDocument()
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
      ["DeleteUserCommand", { UserName: "alice" }],
      ["DeleteUserCommand", { UserName: "bob" }],
    ])
  })
})
