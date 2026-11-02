import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { StateBadge, StatusBadge } from "./state-badge"

describe("StateBadge", () => {
  it.each([
    ["running", "success"],
    ["available", "success"],
    ["associated", "success"],
    ["in-use", "success"],
    ["Complete", "success"],
    ["ACTIVE", "success"],
    ["healthy", "success"],
    ["pending", "progress"],
    ["shutting-down", "progress"],
    ["IN_PROGRESS", "progress"],
    ["draining", "progress"],
    ["DEGRADED", "caution"],
    ["impaired", "caution"],
    ["stopped", "danger"],
    ["FAILED", "danger"],
    ["DELETE_UNSUCCESSFUL", "danger"],
    ["unhealthy", "danger"],
    ["terminated", "muted"],
    ["no", "muted"],
    ["something-else", "neutral"],
  ])("gives '%s' the %s tone", (state, tone) => {
    render(<StateBadge state={state} />)
    expect(
      screen.getByText(state.toLowerCase().replaceAll("_", "-")),
    ).toHaveAttribute("data-tone", tone)
  })

  it("shows every state lowercase, whatever case the API used", () => {
    render(<StateBadge state="DELETE_UNSUCCESSFUL" />)
    expect(screen.getByText("delete-unsuccessful")).toBeInTheDocument()
    expect(screen.queryByText("DELETE_UNSUCCESSFUL")).not.toBeInTheDocument()
  })

  // A Bedrock job the user stopped finished; EC2's stopped is still red.
  it("keeps Bedrock's STOPPED neutral", () => {
    render(<StateBadge state="STOPPED" />)
    expect(screen.getByText("stopped")).toHaveAttribute("data-tone", "neutral")
  })

  it("spins while the state is changing, and only then", () => {
    const { container, rerender } = render(<StateBadge state="pending" />)
    expect(
      container.querySelector('[data-icon="progress"]'),
    ).toBeInTheDocument()

    rerender(<StateBadge state="running" />)
    expect(container.querySelector('[data-icon="progress"]')).toBeNull()
  })

  it("renders neutral for undefined state", () => {
    const { container } = render(<StateBadge state={undefined} />)
    expect(container.firstElementChild).toHaveAttribute("data-tone", "neutral")
  })
})

describe("StatusBadge", () => {
  it("takes its tone from the caller, for a status that is not an API state", () => {
    render(<StatusBadge tone="success">Healthy</StatusBadge>)
    expect(screen.getByText("Healthy")).toHaveAttribute("data-tone", "success")
  })
})
