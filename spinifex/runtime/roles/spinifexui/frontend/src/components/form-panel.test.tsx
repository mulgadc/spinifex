import { act, fireEvent, render, screen } from "@testing-library/react"
import { useState } from "react"
import { describe, expect, it, vi } from "vitest"

import { DetailPanel } from "./detail-panel"
import { FormPanel } from "./form-panel"

function Broken(): never {
  throw new Error("AttachVolume refused")
}

function ResourceWithForm({ onPanelClose }: { onPanelClose: () => void }) {
  const [open, setOpen] = useState(false)

  return (
    <DetailPanel
      closeLabel="Close"
      label="Volume details"
      onClose={onPanelClose}
      title="vol-1"
    >
      <button
        onClick={() => {
          setOpen(true)
        }}
        type="button"
      >
        Attach
      </button>
      {open && (
        <FormPanel
          onClose={() => {
            setOpen(false)
          }}
          subtitle="vol-1"
          title="Attach volume"
        >
          <input aria-label="Device" />
        </FormPanel>
      )}
    </DetailPanel>
  )
}

describe("FormPanel", () => {
  it("takes Escape from the panel it lies over", () => {
    vi.useFakeTimers()
    const onPanelClose = vi.fn()
    render(<ResourceWithForm onPanelClose={onPanelClose} />)

    fireEvent.click(screen.getByRole("button", { name: "Attach" }))
    fireEvent.keyDown(screen.getByRole("textbox", { name: "Device" }), {
      key: "Escape",
    })
    act(() => {
      vi.advanceTimersByTime(200)
    })

    expect(screen.queryByRole("region", { name: "Attach volume" })).toBeNull()
    expect(onPanelClose).not.toHaveBeenCalled()
    vi.useRealTimers()
  })

  it("takes focus on open and hands it back to its button on close", () => {
    render(<ResourceWithForm onPanelClose={vi.fn()} />)
    const attach = screen.getByRole("button", { name: "Attach" })
    attach.focus()

    fireEvent.click(attach)
    expect(screen.getByRole("heading", { name: "Attach volume" })).toHaveFocus()

    fireEvent.click(screen.getByRole("button", { name: "Close Attach volume" }))
    expect(attach).toHaveFocus()
  })

  it("keeps a failure inside the form", () => {
    vi.spyOn(console, "error").mockReturnValue()

    render(
      <FormPanel onClose={vi.fn()} title="Attach volume">
        <Broken />
      </FormPanel>,
    )

    expect(
      screen.getByRole("region", { name: "Attach volume" }),
    ).toHaveTextContent("AttachVolume refused")
  })
})
