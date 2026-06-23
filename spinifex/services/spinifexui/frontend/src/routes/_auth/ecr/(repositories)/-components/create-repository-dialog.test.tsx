import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { send } = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock("@/lib/awsClient", () => ({
  getEcrClient: () => ({ send }),
}))

import { CreateRepositoryDialog } from "./create-repository-dialog"

describe("CreateRepositoryDialog", () => {
  it("disables create until a name is entered, then sends the command", async () => {
    send.mockResolvedValue({})
    const onOpenChange = vi.fn()
    renderWithClient(
      <CreateRepositoryDialog onOpenChange={onOpenChange} open={true} />,
      createTestQueryClient(),
    )

    const createButton = screen.getByRole("button", {
      name: "Create Repository",
    })
    expect(createButton).toBeDisabled()

    fireEvent.change(screen.getByPlaceholderText("team/app"), {
      target: { value: "team/app" },
    })
    expect(createButton).toBeEnabled()

    fireEvent.click(createButton)
    await waitFor(() => expect(send).toHaveBeenCalledTimes(1))
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      repositoryName: "team/app",
    })
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
  })

  it("surfaces a create error and stays open", async () => {
    send.mockRejectedValue(new Error("RepositoryAlreadyExists"))
    renderWithClient(
      <CreateRepositoryDialog onOpenChange={vi.fn()} open={true} />,
      createTestQueryClient(),
    )
    fireEvent.change(screen.getByPlaceholderText("team/app"), {
      target: { value: "dupe" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Create Repository" }))
    expect(
      await screen.findByText("RepositoryAlreadyExists"),
    ).toBeInTheDocument()
  })
})
