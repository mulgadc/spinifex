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

import { DeleteDBSnapshotDialog } from "./delete-db-snapshot-dialog"

const ID = "orders-db-snapshot-20260817-1432"

function render(onDeleted = vi.fn()) {
  return renderWithClient(
    <DeleteDBSnapshotDialog
      dbSnapshotIdentifier={ID}
      onDeleted={onDeleted}
      onOpenChange={vi.fn()}
      open={true}
    />,
    createTestQueryClient(),
  )
}

describe("DeleteDBSnapshotDialog", () => {
  it("states both refusals before anything is sent", () => {
    render()
    expect(
      screen.getByText(/refused while an instance restored from it still/),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/releases the data volume its instance left behind/),
    ).toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("sends DeleteDBSnapshot on confirm", async () => {
    const onDeleted = vi.fn()
    render(onDeleted)
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBSnapshotIdentifier: ID,
    })
    await waitFor(() => {
      expect(onDeleted).toHaveBeenCalled()
    })
  })

  // The refusal names the instance still reading through the snapshot, which
  // is the useful half of that failure.
  it("surfaces the refusal when the delete fails", async () => {
    mockSend.mockRejectedValueOnce(
      new Error(`Snapshot ${ID} is in use by orders-db-restored`),
    )
    const onDeleted = vi.fn()
    render(onDeleted)
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await expect(
      screen.findByText(/is in use by orders-db-restored/),
    ).resolves.toBeInTheDocument()
    expect(onDeleted).not.toHaveBeenCalled()
  })
})
