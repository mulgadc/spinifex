import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { iamGroupQueryOptions, iamUsersQueryOptions } from "@/queries/iam"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const { send } = vi.hoisted(() => ({
  send: vi.fn<(command: { input: object }) => Promise<unknown>>(
    async () => ({}),
  ),
}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { GroupDetails } from "./group-details"

const CREATED = new Date("2026-01-01T00:00:00Z")

function iamUser(userName: string) {
  return {
    UserName: userName,
    UserId: `AIDA${userName}`,
    Arn: `arn:aws:iam::000000000001:user/${userName}`,
    Path: "/",
    CreateDate: CREATED,
  }
}

// The panel's own reads go through the same client, so pick out the removals.
function removals() {
  return send.mock.calls
    .map(([command]) => command.input)
    .filter((input) => "UserName" in input && "GroupName" in input)
}

function renderOverview() {
  const qc = createTestQueryClient()
  qc.setQueryData(iamGroupQueryOptions("devs").queryKey, {
    $metadata: {},
    Group: undefined,
    Users: [iamUser("alice")],
  })
  qc.setQueryData(iamUsersQueryOptions.queryKey, {
    $metadata: {},
    Users: [iamUser("alice"), iamUser("bob")],
  })
  renderWithClient(
    <GroupDetails
      group={{
        GroupName: "devs",
        GroupId: "AGPA1",
        Arn: "arn:aws:iam::000000000001:group/devs",
        Path: "/",
        CreateDate: CREATED,
      }}
      layout="page"
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      tab="overview"
    />,
    qc,
  )
}

describe("GroupDetails members", () => {
  it("links each member to the user's page", async () => {
    renderOverview()

    await expect(
      screen.findByRole("link", { name: "alice" }),
    ).resolves.toHaveAttribute("href", "/iam/list-users/alice")
  })

  it("offers only users who are not already members", async () => {
    const user = userEvent.setup()
    renderOverview()

    await user.click(await screen.findByRole("button", { name: "Add user" }))

    expect(screen.getByRole("button", { name: /^bob/u })).toBeInTheDocument()
    expect(
      screen.queryByRole("button", { name: /^alice/u }),
    ).not.toBeInTheDocument()
  })

  it("removes a member only once the removal is confirmed", async () => {
    const user = userEvent.setup()
    renderOverview()

    await user.click(await screen.findByRole("button", { name: "Remove" }))
    expect(removals()).toHaveLength(0)

    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Remove" }))

    await waitFor(() => {
      expect(removals()).toHaveLength(1)
    })
    expect(removals()[0]).toStrictEqual({
      GroupName: "devs",
      UserName: "alice",
    })
  })
})
