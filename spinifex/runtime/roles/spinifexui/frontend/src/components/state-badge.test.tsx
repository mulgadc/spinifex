import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { StateBadge } from "./state-badge"

describe("StateBadge", () => {
  it.each(["running", "available", "completed", "Active"])(
    "renders green for success state '%s'",
    (state) => {
      render(<StateBadge state={state} />)
      const badge = screen.getByText(state)
      expect(badge.className).toContain("bg-green-100")
    },
  )

  it.each(["stopped", "error"])("renders red for error state '%s'", (state) => {
    render(<StateBadge state={state} />)
    const badge = screen.getByText(state)
    expect(badge.className).toContain("bg-red-100")
  })

  it.each(["pending", "shutting-down", "stopping"])(
    "renders yellow for warning state '%s'",
    (state) => {
      render(<StateBadge state={state} />)
      const badge = screen.getByText(state)
      expect(badge.className).toContain("bg-yellow-100")
    },
  )

  it("renders gray for terminated state", () => {
    render(<StateBadge state="terminated" />)
    const badge = screen.getByText("terminated")
    expect(badge.className).toContain("bg-zinc-100")
  })

  it("renders default gray for unknown state", () => {
    render(<StateBadge state="something-else" />)
    const badge = screen.getByText("something-else")
    expect(badge.className).toContain("bg-gray-100")
  })

  it("renders default gray for undefined state", () => {
    const { container } = render(<StateBadge state={undefined} />)
    const badge = container.firstElementChild
    expect(badge?.className).toContain("bg-gray-100")
  })
})
