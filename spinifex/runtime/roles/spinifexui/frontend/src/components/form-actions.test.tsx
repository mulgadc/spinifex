import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { FormActions } from "./form-actions"

describe("FormActions", () => {
  it("renders submit and cancel buttons", () => {
    render(
      <FormActions
        isPending={false}
        isSubmitting={false}
        onCancel={vi.fn()}
        pendingLabel="Creating..."
        submitLabel="Create"
      />,
    )
    expect(screen.getByRole("button", { name: "Create" })).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument()
  })

  it("shows pending label when submitting", () => {
    render(
      <FormActions
        isPending={false}
        isSubmitting={true}
        onCancel={vi.fn()}
        pendingLabel="Creating..."
        submitLabel="Create"
      />,
    )
    expect(
      screen.getByRole("button", { name: "Creating..." }),
    ).toBeInTheDocument()
  })

  it("shows pending label when isPending", () => {
    render(
      <FormActions
        isPending={true}
        isSubmitting={false}
        onCancel={vi.fn()}
        pendingLabel="Creating..."
        submitLabel="Create"
      />,
    )
    expect(
      screen.getByRole("button", { name: "Creating..." }),
    ).toBeInTheDocument()
  })

  it("disables both buttons when submitting", () => {
    render(
      <FormActions
        isPending={false}
        isSubmitting={true}
        onCancel={vi.fn()}
        pendingLabel="Creating..."
        submitLabel="Create"
      />,
    )
    expect(screen.getByRole("button", { name: "Creating..." })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled()
  })

  it("calls onCancel when cancel is clicked", async () => {
    const onCancel = vi.fn()
    const user = userEvent.setup()
    render(
      <FormActions
        isPending={false}
        isSubmitting={false}
        onCancel={onCancel}
        pendingLabel="Creating..."
        submitLabel="Create"
      />,
    )
    await user.click(screen.getByRole("button", { name: "Cancel" }))
    expect(onCancel).toHaveBeenCalledOnce()
  })
})
