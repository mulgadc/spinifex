import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { send } = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

import { CreateClusterPage } from "./create-cluster-page"

const onClose = vi.fn()

function renderPage() {
  renderWithClient(
    <CreateClusterPage onClose={onClose} />,
    createTestQueryClient(),
  )
}

describe("CreateClusterPage", () => {
  beforeEach(() => {
    send.mockReset()
    onClose.mockReset()
  })

  it("refuses an empty name without calling ECS", async () => {
    const user = userEvent.setup()
    renderPage()

    await user.click(screen.getByRole("button", { name: "Create cluster" }))

    await expect(
      screen.findByText("Enter a cluster name"),
    ).resolves.toBeInTheDocument()
    expect(send).not.toHaveBeenCalled()
  })

  it("creates the cluster and closes with its name", async () => {
    const user = userEvent.setup()
    send.mockResolvedValue({ cluster: { clusterName: "web" } })
    renderPage()

    await user.type(screen.getByLabelText("Name"), "web")

    await user.click(screen.getByRole("button", { name: "Create cluster" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("web")
    })
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      clusterName: "web",
    })
  })

  it("keeps the form open with the error beside the button", async () => {
    const user = userEvent.setup()
    send.mockRejectedValue(new Error("ClusterAlreadyExists"))
    renderPage()

    await user.type(screen.getByLabelText("Name"), "dupe")
    await user.click(screen.getByRole("button", { name: "Create cluster" }))

    await expect(
      screen.findByText("Failed to create cluster"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByLabelText("Name")).toBeEnabled()
    expect(onClose).not.toHaveBeenCalled()
  })
})
