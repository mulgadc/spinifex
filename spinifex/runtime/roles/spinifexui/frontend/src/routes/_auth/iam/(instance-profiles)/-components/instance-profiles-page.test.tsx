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

import { InstanceProfilesPage } from "./instance-profiles-page"

const PROFILES = [
  {
    InstanceProfileName: "web",
    InstanceProfileId: "AIPA1",
    Arn: "arn:aws:iam::000000000001:instance-profile/web",
    Path: "/",
    CreateDate: new Date("2026-01-01T00:00:00Z"),
    Roles: [],
  },
  {
    InstanceProfileName: "batch",
    InstanceProfileId: "AIPA2",
    Arn: "arn:aws:iam::000000000001:instance-profile/batch",
    Path: "/",
    CreateDate: new Date("2026-01-01T00:00:00Z"),
    Roles: [],
  },
]

describe("InstanceProfilesPage", () => {
  it("deletes the selected instance profiles, one call each", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <InstanceProfilesPage
        onSearchChange={vi.fn()}
        instanceProfiles={PROFILES}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select web" }))
    await user.click(screen.getByRole("checkbox", { name: "Select batch" }))
    await user.click(screen.getByRole("button", { name: "Delete" }))
    expect(screen.getByText("batch (AIPA2)")).toBeInTheDocument()
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
      ["DeleteInstanceProfileCommand", { InstanceProfileName: "web" }],
      ["DeleteInstanceProfileCommand", { InstanceProfileName: "batch" }],
    ])
  })
})
