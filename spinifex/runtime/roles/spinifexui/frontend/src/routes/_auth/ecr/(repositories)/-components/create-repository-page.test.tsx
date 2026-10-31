import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { send } = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

import { CreateRepositoryPage } from "./create-repository-page"

const onClose = vi.fn()

function renderPage() {
  renderWithClient(
    <CreateRepositoryPage onClose={onClose} />,
    createTestQueryClient(),
  )
}

describe("CreateRepositoryPage", () => {
  beforeEach(() => {
    send.mockReset()
    onClose.mockReset()
  })

  it("refuses an empty name without calling ECR", async () => {
    const user = userEvent.setup()
    renderPage()

    await user.click(screen.getByRole("button", { name: "Create repository" }))

    await expect(
      screen.findByText("Enter a repository name"),
    ).resolves.toBeInTheDocument()
    expect(send).not.toHaveBeenCalled()
  })

  it("creates the repository and closes with its name", async () => {
    const user = userEvent.setup()
    send.mockResolvedValue({ repository: { repositoryName: "team/app" } })
    renderPage()

    await user.type(screen.getByLabelText("Name"), "team/app")

    await user.click(screen.getByRole("button", { name: "Create repository" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("team/app")
    })
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      repositoryName: "team/app",
    })
  })

  it("keeps the form open with the error beside the button", async () => {
    const user = userEvent.setup()
    send.mockRejectedValue(new Error("RepositoryAlreadyExists"))
    renderPage()

    await user.type(screen.getByLabelText("Name"), "dupe")
    await user.click(screen.getByRole("button", { name: "Create repository" }))

    await expect(
      screen.findByText("Failed to create repository"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByLabelText("Name")).toBeEnabled()
    expect(onClose).not.toHaveBeenCalled()
  })
})
