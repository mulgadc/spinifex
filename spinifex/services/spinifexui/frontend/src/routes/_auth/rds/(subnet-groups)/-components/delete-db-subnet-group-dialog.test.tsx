import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

import { DeleteDBSubnetGroupDialog } from "./delete-db-subnet-group-dialog"

function render() {
  return renderWithClient(
    <DeleteDBSubnetGroupDialog
      dbSubnetGroupName="orders-subnets"
      onOpenChange={vi.fn()}
      open={true}
    />,
    createTestQueryClient(),
  )
}

describe("DeleteDBSubnetGroupDialog", () => {
  it("states the in-use refusal before anything is sent", () => {
    render()
    expect(
      screen.getByText(/refused while any DB instance still references it/),
    ).toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("sends DeleteDBSubnetGroup on confirm", async () => {
    render()
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBSubnetGroupName: "orders-subnets",
    })
  })

  // The refusal names every instance holding the group, which is the useful
  // half of the failure, so it must reach the screen rather than vanish.
  it("surfaces the refusal when the delete fails", async () => {
    mockSend.mockRejectedValueOnce(
      new Error("DB subnet group orders-subnets is still used by orders-db"),
    )
    render()
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    expect(
      await screen.findByText(/is still used by orders-db/),
    ).toBeInTheDocument()
  })
})
