import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn(async () => ({})))
})

import { CreatePlacementGroupPage } from "./create-placement-group-page"

describe("CreatePlacementGroupPage", () => {
  it("explains both strategies beside the picker", async () => {
    renderWithClient(
      <CreatePlacementGroupPage onClose={vi.fn()} />,
      createTestQueryClient(),
    )

    await userEvent.click(
      screen.getByRole("button", { name: "About Strategy" }),
    )

    await expect(
      screen.findByText(/keeps every instance on one node/u),
    ).resolves.toBeInTheDocument()
    expect(
      screen.getByText(/each instance on a different node/u),
    ).toBeInTheDocument()
  })
})
