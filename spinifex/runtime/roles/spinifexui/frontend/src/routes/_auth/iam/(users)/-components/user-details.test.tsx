import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { iamAccessKeysQueryOptions } from "@/queries/iam"
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

import { UserDetails } from "./user-details"

// The panel's own reads go through the same client, so pick out the key calls.
function keyCalls() {
  return send.mock.calls
    .map(([command]) => command.input)
    .filter((input) => "AccessKeyId" in input)
}

const USER = {
  UserName: "alice",
  UserId: "AIDA1",
  Arn: "arn:aws:iam::000000000001:user/alice",
  Path: "/",
  CreateDate: new Date("2026-01-01T00:00:00Z"),
}

function renderSettings() {
  const qc = createTestQueryClient()
  qc.setQueryData(iamAccessKeysQueryOptions("alice").queryKey, {
    $metadata: {},
    AccessKeyMetadata: [{ AccessKeyId: "AKIA1", Status: "Active" }],
  })
  renderWithClient(
    <UserDetails
      layout="page"
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      tab="settings"
      user={USER}
    />,
    qc,
  )
}

describe("UserDetails access keys", () => {
  it("deletes a key only once the delete is confirmed", async () => {
    const user = userEvent.setup()
    renderSettings()

    await user.click(
      await screen.findByRole("button", { name: "Delete access key AKIA1" }),
    )
    expect(keyCalls()).toHaveLength(0)

    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(keyCalls()).toHaveLength(1)
    })
    expect(keyCalls()[0]).toStrictEqual({
      UserName: "alice",
      AccessKeyId: "AKIA1",
    })
  })

  it("deactivates an active key as Inactive after the confirm", async () => {
    const user = userEvent.setup()
    renderSettings()

    await user.click(await screen.findByRole("button", { name: "Deactivate" }))
    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Deactivate" }))

    await waitFor(() => {
      expect(keyCalls()).toHaveLength(1)
    })
    expect(keyCalls()[0]).toMatchObject({
      UserName: "alice",
      AccessKeyId: "AKIA1",
      Status: "Inactive",
    })
  })
})
