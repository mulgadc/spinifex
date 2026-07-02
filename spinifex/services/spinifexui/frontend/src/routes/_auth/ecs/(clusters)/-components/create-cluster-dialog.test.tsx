import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { send } = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock("@/lib/awsClient", () => ({
  getEcsClient: () => ({ send }),
}))

import { CreateClusterDialog } from "./create-cluster-dialog"

describe("CreateClusterDialog", () => {
  it("disables create until a name is entered, then sends the command", async () => {
    send.mockResolvedValue({})
    const onOpenChange = vi.fn()
    renderWithClient(
      <CreateClusterDialog onOpenChange={onOpenChange} open={true} />,
      createTestQueryClient(),
    )

    const createButton = screen.getByRole("button", { name: "Create Cluster" })
    expect(createButton).toBeDisabled()

    fireEvent.change(screen.getByPlaceholderText("my-cluster"), {
      target: { value: "web" },
    })
    expect(createButton).toBeEnabled()

    fireEvent.click(createButton)
    await waitFor(() => expect(send).toHaveBeenCalledTimes(1))
    expect(send.mock.calls[0]![0].input).toStrictEqual({ clusterName: "web" })
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
  })

  it("surfaces a create error and stays open", async () => {
    send.mockRejectedValue(new Error("ClusterAlreadyExists"))
    renderWithClient(
      <CreateClusterDialog onOpenChange={vi.fn()} open={true} />,
      createTestQueryClient(),
    )
    fireEvent.change(screen.getByPlaceholderText("my-cluster"), {
      target: { value: "dupe" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Create Cluster" }))
    expect(await screen.findByText("ClusterAlreadyExists")).toBeInTheDocument()
  })
})
