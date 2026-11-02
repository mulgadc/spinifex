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

// Both labels stay rendered, so the button keeps one width through the swap.
function labels(isPending: boolean) {
  render(
    <FormActions
      isPending={isPending}
      isSubmitting={false}
      onCancel={vi.fn()}
      pendingLabel="Creating..."
      submitLabel="Create"
    />,
  )

  const button = screen.getByRole("button", { name: /Creat/u })

  return {
    submit: button.querySelector('[data-label="submit"]'),
    pending: button.querySelector('[data-label="pending"]'),
  }
}

describe("FormActions pending transition", () => {
  it("keeps both labels in the button and fades the idle one out", () => {
    const { submit, pending } = labels(false)
    expect(submit).not.toHaveClass("opacity-0")
    expect(pending).toHaveClass("opacity-0")
    expect(pending).toHaveAttribute("aria-hidden", "true")
  })

  it("fades the submit label out while pending", () => {
    const { submit, pending } = labels(true)
    expect(submit).toHaveClass("opacity-0")
    expect(submit).toHaveAttribute("aria-hidden", "true")
    expect(pending).not.toHaveClass("opacity-0")
  })
})
