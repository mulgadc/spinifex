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

import { PoliciesPage } from "./policies-page"

const POLICIES = [
  {
    PolicyName: "read-only",
    PolicyId: "ANPA1",
    Arn: "arn:aws:iam::000000000001:policy/read-only",
    AttachmentCount: 0,
    UpdateDate: new Date("2026-01-01T00:00:00Z"),
  },
  {
    PolicyName: "deploy",
    PolicyId: "ANPA2",
    Arn: "arn:aws:iam::000000000001:policy/deploy",
    AttachmentCount: 0,
    UpdateDate: new Date("2026-01-01T00:00:00Z"),
  },
]

describe("PoliciesPage", () => {
  it("deletes the selected policies, one call each", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <PoliciesPage
        onSearchChange={vi.fn()}
        policies={POLICIES}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(
      screen.getByRole("checkbox", {
        name: "Select arn:aws:iam::000000000001:policy/read-only",
      }),
    )
    await user.click(
      screen.getByRole("checkbox", {
        name: "Select arn:aws:iam::000000000001:policy/deploy",
      }),
    )
    await user.click(screen.getByRole("button", { name: "Delete" }))
    expect(
      screen.getByText("deploy (arn:aws:iam::000000000001:policy/deploy)"),
    ).toBeInTheDocument()
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
      [
        "DeletePolicyCommand",
        { PolicyArn: "arn:aws:iam::000000000001:policy/read-only" },
      ],
      [
        "DeletePolicyCommand",
        { PolicyArn: "arn:aws:iam::000000000001:policy/deploy" },
      ],
    ])
  })
})
