import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { SystemImageRequired } from "./system-image-required"

const importCommand = "spx admin images import --name spinifex-ecs-node"

function renderCallout(
  overrides: Partial<Parameters<typeof SystemImageRequired>[0]> = {},
) {
  const onRecheck = vi.fn()
  const { container } = render(
    <SystemImageRequired
      description="Import it before creating a cluster."
      importCommand={importCommand}
      isRechecking={false}
      onRecheck={onRecheck}
      title="ECS system image not found"
      {...overrides}
    />,
  )
  return { container, onRecheck }
}

describe("SystemImageRequired", () => {
  it("renders the title, description and import command", () => {
    renderCallout()

    expect(screen.getByText("ECS system image not found")).toBeInTheDocument()
    expect(
      screen.getByText("Import it before creating a cluster."),
    ).toBeInTheDocument()
    expect(screen.getByText(importCommand)).toBeInTheDocument()
  })

  it("copies the import command to the clipboard", async () => {
    const user = userEvent.setup()
    renderCallout()

    await user.click(screen.getByRole("button", { name: "Copy command" }))

    expect(await navigator.clipboard.readText()).toBe(importCommand)
  })

  it("calls onRecheck when Recheck is clicked", async () => {
    const user = userEvent.setup()
    const { onRecheck } = renderCallout()

    await user.click(screen.getByRole("button", { name: /recheck/i }))

    expect(onRecheck).toHaveBeenCalledOnce()
  })

  it("disables Recheck while a recheck is in flight", () => {
    renderCallout({ isRechecking: true })

    expect(screen.getByRole("button", { name: /recheck/i })).toBeDisabled()
  })

  it("applies the call site's className alongside the base styles", () => {
    const { container } = renderCallout({ className: "mb-6" })

    expect(container.firstChild).toHaveClass("mb-6")
    expect(container.firstChild).toHaveClass("max-w-2xl")
  })

  it("omits call site spacing when no className is given", () => {
    const { container } = renderCallout()

    expect(container.firstChild).not.toHaveClass("mb-6")
  })
})
