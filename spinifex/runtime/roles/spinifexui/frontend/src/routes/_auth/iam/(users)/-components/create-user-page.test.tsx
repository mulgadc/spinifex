import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { CreateUserPage } from "./create-user-page"

const onClose = vi.fn()

function submit(name: string) {
  fireEvent.change(screen.getByLabelText("User Name"), {
    target: { value: name },
  })
  fireEvent.click(screen.getByRole("button", { name: "Create User" }))
}

describe("CreateUserPage", () => {
  it("creates it and closes with the new name", async () => {
    mockSend.mockResolvedValueOnce({ User: { UserName: "deploy-bot" } })
    renderWithClient(
      <CreateUserPage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("deploy-bot")

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("deploy-bot")
    })
    expect(mockSend.mock.calls[0]?.[0].input.UserName).toBe("deploy-bot")
  })

  it("shows a refused create beside the button and stays open", async () => {
    mockSend.mockRejectedValueOnce(
      Object.assign(new Error("User with name deploy-bot already exists."), {
        name: "EntityAlreadyExists",
      }),
    )
    renderWithClient(
      <CreateUserPage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("deploy-bot")

    await expect(
      screen.findByText(
        "EntityAlreadyExists: User with name deploy-bot already exists.",
      ),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByRole("button", { name: "Create User" })).toBeEnabled()
  })
})
