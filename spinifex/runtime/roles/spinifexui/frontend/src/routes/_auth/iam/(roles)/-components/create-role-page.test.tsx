import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { CreateRolePage } from "./create-role-page"

const onClose = vi.fn()

function submit(name: string) {
  fireEvent.change(screen.getByLabelText("Role Name"), {
    target: { value: name },
  })
  fireEvent.click(screen.getByRole("button", { name: "Create Role" }))
}

describe("CreateRolePage", () => {
  it("creates it and closes with the new name", async () => {
    mockSend.mockResolvedValueOnce({ Role: { RoleName: "app-role" } })
    renderWithClient(
      <CreateRolePage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("app-role")

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("app-role")
    })
    expect(mockSend.mock.calls[0]?.[0].input.RoleName).toBe("app-role")
  })

  it("shows a refused create beside the button and stays open", async () => {
    mockSend.mockRejectedValueOnce(
      Object.assign(new Error("Role with name app-role already exists."), {
        name: "EntityAlreadyExists",
      }),
    )
    renderWithClient(
      <CreateRolePage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("app-role")

    await expect(
      screen.findByText(
        "EntityAlreadyExists: Role with name app-role already exists.",
      ),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByRole("button", { name: "Create Role" })).toBeEnabled()
  })
})
