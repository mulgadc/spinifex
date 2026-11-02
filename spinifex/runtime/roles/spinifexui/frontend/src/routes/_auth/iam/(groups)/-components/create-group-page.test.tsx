import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { CreateGroupPage } from "./create-group-page"

const onClose = vi.fn()

function submit(name: string) {
  fireEvent.change(screen.getByLabelText("Group Name"), {
    target: { value: name },
  })
  fireEvent.click(screen.getByRole("button", { name: "Create Group" }))
}

describe("CreateGroupPage", () => {
  it("creates it and closes with the new name", async () => {
    mockSend.mockResolvedValueOnce({ Group: { GroupName: "developers" } })
    renderWithClient(
      <CreateGroupPage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("developers")

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("developers")
    })
    expect(mockSend.mock.calls[0]?.[0].input.GroupName).toBe("developers")
  })

  it("shows a refused create beside the button and stays open", async () => {
    mockSend.mockRejectedValueOnce(
      Object.assign(new Error("Group with name developers already exists."), {
        name: "EntityAlreadyExists",
      }),
    )
    renderWithClient(
      <CreateGroupPage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("developers")

    await expect(
      screen.findByText(
        "EntityAlreadyExists: Group with name developers already exists.",
      ),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByRole("button", { name: "Create Group" })).toBeEnabled()
  })
})
