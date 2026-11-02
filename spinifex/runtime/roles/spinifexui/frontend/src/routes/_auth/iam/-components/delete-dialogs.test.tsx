import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactElement } from "react"
import { describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL } from "@/lib/panel-search"
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

import { GroupDetails } from "../(groups)/-components/group-details"
import { InstanceProfileDetails } from "../(instance-profiles)/-components/instance-profile-details"
import { PolicyDetails } from "../(policies)/-components/policy-details"
import { RoleDetails } from "../(roles)/-components/role-details"
import { UserDetails } from "../(users)/-components/user-details"
import { UsersPage } from "../(users)/-components/users-page"

const panel = {
  layout: "page",
  onClose: vi.fn(),
  onTabChange: vi.fn(),
} as const

function entity(kind: string, name: string) {
  return {
    Path: "/",
    Arn: `arn:aws:iam::000000000001:${kind}/${name}`,
    CreateDate: new Date("2026-01-01T00:00:00Z"),
  }
}

const CASES: [string, ReactElement, string][] = [
  [
    "Delete user",
    <UserDetails
      {...panel}
      key="user"
      tab="settings"
      user={{ ...entity("user", "alice"), UserName: "alice", UserId: "AIDA1" }}
    />,
    "Delete the user alice? Detach its policies, delete its access keys and remove it from its groups first, or the delete is refused. This cannot be undone.",
  ],
  [
    "Delete group",
    <GroupDetails
      {...panel}
      group={{
        ...entity("group", "devs"),
        GroupName: "devs",
        GroupId: "AGPA1",
      }}
      key="group"
      tab="settings"
    />,
    "Delete the group devs? Remove its members and detach its policies first, or the delete is refused. This cannot be undone.",
  ],
  [
    "Delete role",
    <RoleDetails
      {...panel}
      key="role"
      role={{ ...entity("role", "app"), RoleName: "app", RoleId: "AROA1" }}
      tab="settings"
    />,
    "Delete the role app? Detach its policies and remove it from its instance profiles first, or the delete is refused. This cannot be undone.",
  ],
  [
    "Delete policy",
    <PolicyDetails
      {...panel}
      key="policy"
      policy={{ ...entity("policy", "read"), PolicyName: "read" }}
      tab="settings"
    />,
    "Delete the policy read? Detach it from every user, group and role first, or the delete is refused. This cannot be undone.",
  ],
  [
    "Delete instance profile",
    <InstanceProfileDetails
      {...panel}
      key="profile"
      profile={{
        ...entity("instance-profile", "web"),
        InstanceProfileName: "web",
        InstanceProfileId: "AIPA1",
        Roles: [],
      }}
      tab="settings"
    />,
    "Delete the instance profile web? Remove its role first, or the delete is refused. This cannot be undone.",
  ],
]

describe("IAM delete dialogs", () => {
  it.each(CASES)(
    "%s names what AWS refuses the delete over",
    async (button, element, text) => {
      const user = userEvent.setup()
      renderWithClient(element, createTestQueryClient())

      await user.click(await screen.findByRole("button", { name: button }))

      expect(
        within(await screen.findByRole("alertdialog")).getByText(text),
      ).toBeInTheDocument()
    },
  )

  it("words the bulk dialog the same way, for many", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <UsersPage
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
        users={[
          { ...entity("user", "alice"), UserName: "alice", UserId: "AIDA1" },
          { ...entity("user", "bob"), UserName: "bob", UserId: "AIDA2" },
        ]}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select alice" }))
    await user.click(screen.getByRole("checkbox", { name: "Select bob" }))
    await user.click(screen.getByRole("button", { name: "Delete" }))

    expect(
      within(screen.getByRole("alertdialog")).getByText(
        "Detach their policies, delete their access keys and remove them from their groups first, or the delete is refused. This cannot be undone.",
      ),
    ).toBeInTheDocument()
  })
})
