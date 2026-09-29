import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { DeleteConfirmationDialog } from "./delete-confirmation-dialog"

describe("DeleteConfirmationDialog", () => {
  const defaultProps = {
    open: true,
    onOpenChange: vi.fn(),
    title: "Delete instance",
    description: "Are you sure you want to delete this instance?",
    isPending: false,
    onConfirm: vi.fn(),
  }

  it("renders title and description when open", () => {
    render(<DeleteConfirmationDialog {...defaultProps} />)
    expect(screen.getByText("Delete instance")).toBeInTheDocument()
    expect(
      screen.getByText("Are you sure you want to delete this instance?"),
    ).toBeInTheDocument()
  })

  it("renders Cancel and Delete buttons", () => {
    render(<DeleteConfirmationDialog {...defaultProps} />)
    expect(screen.getByText("Cancel")).toBeInTheDocument()
    expect(screen.getByText("Delete")).toBeInTheDocument()
  })

  it("shows 'Deleting...' when isPending is true", () => {
    render(<DeleteConfirmationDialog {...defaultProps} isPending={true} />)
    expect(screen.getByText("Deleting\u2026")).toBeInTheDocument()
  })

  it("disables the confirm button when isPending", () => {
    render(<DeleteConfirmationDialog {...defaultProps} isPending={true} />)
    const deleteBtn = screen.getByText("Deleting\u2026")
    expect(deleteBtn).toBeDisabled()
  })

  it("calls onConfirm when Delete is clicked", async () => {
    const onConfirm = vi.fn()
    const user = userEvent.setup()
    render(<DeleteConfirmationDialog {...defaultProps} onConfirm={onConfirm} />)
    await user.click(screen.getByText("Delete"))
    expect(onConfirm).toHaveBeenCalledOnce()
  })

  it("renders custom confirmLabel and pendingLabel when provided", () => {
    const { rerender } = render(
      <DeleteConfirmationDialog
        {...defaultProps}
        confirmLabel="Terminate"
        pendingLabel="Terminating…"
      />,
    )
    expect(screen.getByText("Terminate")).toBeInTheDocument()
    expect(screen.queryByText("Delete")).not.toBeInTheDocument()
    rerender(
      <DeleteConfirmationDialog
        {...defaultProps}
        confirmLabel="Terminate"
        isPending={true}
        pendingLabel="Terminating…"
      />,
    )
    expect(screen.getByText("Terminating…")).toBeInTheDocument()
  })

  it("renders ReactNode description", () => {
    render(
      <DeleteConfirmationDialog
        {...defaultProps}
        description={
          <span>
            Delete <strong>i-abc123</strong>?
          </span>
        }
      />,
    )
    expect(screen.getByText("i-abc123")).toBeInTheDocument()
  })
})
