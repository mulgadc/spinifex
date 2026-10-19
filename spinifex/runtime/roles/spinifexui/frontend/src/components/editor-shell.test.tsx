import { fireEvent, render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { EditorShell } from "./editor-shell"

function renderShell(
  props: Partial<React.ComponentProps<typeof EditorShell>> = {},
) {
  const onSave = vi.fn()
  const onReset = vi.fn()

  const { container } = render(
    <EditorShell
      dirty={false}
      errorMsg="Failed to save things"
      isPending={false}
      onReset={onReset}
      onSave={onSave}
      successMsg="Things saved."
      {...props}
    >
      <p>body</p>
    </EditorShell>,
  )

  const form = container.querySelector("form")

  if (!form) {
    throw new Error("form not rendered")
  }

  return { form, onReset, onSave }
}

describe("EditorShell", () => {
  it("keeps Reset available but refuses to save an invalid edit, even on submit", () => {
    const { form, onSave } = renderShell({ dirty: true, invalid: true })

    expect(screen.getByRole("button", { name: "Reset" })).toBeEnabled()
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled()

    fireEvent.submit(form)
    expect(onSave).not.toHaveBeenCalled()
  })

  it("does not save when nothing has changed", () => {
    const { form, onSave } = renderShell()

    expect(screen.getByText("No changes")).toBeInTheDocument()
    fireEvent.submit(form)
    expect(onSave).not.toHaveBeenCalled()
  })

  it("saves a valid dirty edit and labels it with the caller's text", async () => {
    const user = userEvent.setup()
    const { onSave } = renderShell({ dirty: true, dirtyLabel: "2 changes" })

    expect(screen.getByText("2 changes")).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Save changes" }))
    expect(onSave).toHaveBeenCalledOnce()
  })

  it("locks both buttons while a save is in flight", () => {
    renderShell({ dirty: true, isPending: true })

    expect(screen.getByRole("button", { name: "Reset" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Saving…" })).toBeDisabled()
  })

  it("hides the success notice while a newer edit is unsaved", () => {
    const { rerender } = render(
      <EditorShell
        dirty={false}
        errorMsg="Failed"
        isPending={false}
        isSuccess
        onReset={() => {}}
        onSave={() => {}}
        successMsg="Things saved."
      >
        <p>body</p>
      </EditorShell>,
    )

    expect(screen.getByText("Things saved.")).toBeInTheDocument()

    rerender(
      <EditorShell
        dirty
        errorMsg="Failed"
        isPending={false}
        isSuccess
        onReset={() => {}}
        onSave={() => {}}
        successMsg="Things saved."
      >
        <p>body</p>
      </EditorShell>,
    )

    expect(screen.queryByText("Things saved.")).not.toBeInTheDocument()
  })

  it("reports a failed save with both the summary and the error detail", () => {
    renderShell({ error: new Error("throttled") })

    const alert = screen.getByRole("alert")
    expect(alert).toHaveTextContent("Failed to save things")
    expect(alert).toHaveTextContent("throttled")
  })
})
