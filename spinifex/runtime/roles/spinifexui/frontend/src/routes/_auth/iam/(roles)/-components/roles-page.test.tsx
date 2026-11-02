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

import { RolesPage } from "./roles-page"

const ROLES = [
  {
    RoleName: "builder",
    RoleId: "AROA1",
    Arn: "arn:aws:iam::000000000001:role/builder",
    Path: "/",
    CreateDate: new Date("2026-01-01T00:00:00Z"),
  },
  {
    RoleName: "runner",
    RoleId: "AROA2",
    Arn: "arn:aws:iam::000000000001:role/runner",
    Path: "/",
    CreateDate: new Date("2026-01-01T00:00:00Z"),
  },
]

describe("RolesPage", () => {
  it("deletes the selected roles, one call each", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <RolesPage
        onSearchChange={vi.fn()}
        roles={ROLES}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select builder" }))
    await user.click(screen.getByRole("checkbox", { name: "Select runner" }))
    await user.click(screen.getByRole("button", { name: "Delete" }))
    expect(screen.getByText("runner (AROA2)")).toBeInTheDocument()
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
      ["DeleteRoleCommand", { RoleName: "builder" }],
      ["DeleteRoleCommand", { RoleName: "runner" }],
    ])
  })
})
