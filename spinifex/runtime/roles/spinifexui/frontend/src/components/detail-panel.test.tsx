import { act, fireEvent, render, screen } from "@testing-library/react"
import { Eye, SlidersHorizontal } from "lucide-react"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { DetailPanel, PanelField, PanelFields } from "./detail-panel"

function Broken(): never {
  throw new Error("DescribeSubnets refused")
}

describe("DetailPanel", () => {
  beforeEach(() => {
    vi.spyOn(console, "error").mockReturnValue()
  })

  it("slides out on Escape, then closes", () => {
    vi.useFakeTimers()
    const onClose = vi.fn()
    render(
      <DetailPanel
        closeLabel="Close"
        label="Details"
        onClose={onClose}
        title="vpc-1"
      >
        body
      </DetailPanel>,
    )

    fireEvent.keyDown(document, { key: "Escape" })
    expect(screen.getByRole("region", { name: "Details" })).toHaveAttribute(
      "data-closing",
    )
    expect(onClose).not.toHaveBeenCalled()

    act(() => {
      vi.advanceTimersByTime(150)
    })
    expect(onClose).toHaveBeenCalledOnce()
    vi.useRealTimers()
  })

  it("leaves Escape to a dialog open over it", () => {
    const onClose = vi.fn()
    render(
      <>
        <DetailPanel
          closeLabel="Close"
          label="Details"
          onClose={onClose}
          title="vpc-1"
        >
          body
        </DetailPanel>
        <div role="alertdialog">Delete?</div>
      </>,
    )

    fireEvent.keyDown(document, { key: "Escape" })
    expect(onClose).not.toHaveBeenCalled()
  })

  it("keeps a failure inside the panel", () => {
    render(
      <DetailPanel
        closeLabel="Close"
        label="Details"
        onClose={vi.fn()}
        title="vpc-1"
      >
        <Broken />
      </DetailPanel>,
    )

    expect(screen.getByText("This panel could not load")).toBeInTheDocument()
    expect(screen.getByRole("heading", { name: "vpc-1" })).toBeInTheDocument()
  })

  it("shows the requested tab and reports a change", () => {
    const onTabChange = vi.fn()
    render(
      <DetailPanel
        closeLabel="Close"
        label="Details"
        onClose={vi.fn()}
        onTabChange={onTabChange}
        tab="settings"
        tabs={[
          {
            id: "overview",
            label: "Overview",
            icon: Eye,
            content: "Overview body",
          },
          {
            id: "settings",
            label: "Settings",
            icon: SlidersHorizontal,
            content: "Settings body",
          },
        ]}
        title="vpc-1"
      />,
    )

    expect(screen.getByText("Settings body")).toBeVisible()
    fireEvent.click(screen.getByRole("tab", { name: "Overview" }))
    expect(onTabChange).toHaveBeenCalledExactlyOnceWith("overview")
  })
})

describe("PanelField", () => {
  it("copies its value", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    })

    render(
      <PanelFields>
        <PanelField copy="vpc-0abc" label="VPC ID" mono>
          vpc-0abc
        </PanelField>
      </PanelFields>,
    )

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Copy VPC ID" }))
    })

    expect(writeText).toHaveBeenCalledExactlyOnceWith("vpc-0abc")
    expect(
      screen.getByRole("button", { name: "VPC ID copied" }),
    ).toBeInTheDocument()
  })

  it("marks a missing value", () => {
    render(
      <PanelFields>
        <PanelField label="Key pair" />
      </PanelFields>,
    )

    expect(screen.getByText("—")).toBeInTheDocument()
    expect(screen.queryByRole("button")).not.toBeInTheDocument()
  })
})
