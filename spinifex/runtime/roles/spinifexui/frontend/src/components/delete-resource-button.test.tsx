import { useMutation } from "@tanstack/react-query"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { navigate, deleteFn } = vi.hoisted(() => ({
  navigate: vi.fn(),
  deleteFn: vi.fn<(id: string) => Promise<void>>(),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { navigate })
})

import { DeleteResourceButton } from "./delete-resource-button"

function Harness() {
  const mutation = useMutation({ mutationFn: deleteFn })

  return (
    <DeleteResourceButton
      description="This deletes vpc-1."
      id="vpc-1"
      mutation={mutation}
      title="Delete VPC"
      to="/ec2/describe-vpcs"
    />
  )
}

async function openAndConfirm() {
  const user = userEvent.setup()
  renderWithClient(<Harness />, createTestQueryClient())
  await user.click(screen.getByRole("button", { name: "Delete" }))
  expect(screen.getByText("This deletes vpc-1.")).toBeInTheDocument()
  expect(deleteFn).not.toHaveBeenCalled()
  await user.click(screen.getByRole("button", { name: "Delete" }))

  return user
}

describe("DeleteResourceButton", () => {
  beforeEach(() => {
    navigate.mockReset()
    deleteFn.mockReset()
  })

  it("deletes only after confirmation, then navigates to the list", async () => {
    deleteFn.mockResolvedValue()

    await openAndConfirm()

    await waitFor(() => {
      expect(navigate).toHaveBeenCalledWith({ to: "/ec2/describe-vpcs" })
    })
    expect(deleteFn).toHaveBeenCalledWith("vpc-1", expect.anything())
    expect(screen.queryByText("This deletes vpc-1.")).not.toBeInTheDocument()
  })

  it("keeps the dialog open with the refusal when the delete fails", async () => {
    deleteFn.mockRejectedValue(new Error("vpc-1 has dependent subnets"))

    const user = await openAndConfirm()

    await expect(
      screen.findByText("vpc-1 has dependent subnets"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByText("This deletes vpc-1.")).toBeInTheDocument()
    expect(navigate).not.toHaveBeenCalled()

    // Closing clears the refusal so reopening starts clean.
    await user.click(screen.getByRole("button", { name: "Cancel" }))
    await user.click(screen.getByRole("button", { name: "Delete" }))
    expect(
      screen.queryByText("vpc-1 has dependent subnets"),
    ).not.toBeInTheDocument()
  })
})
