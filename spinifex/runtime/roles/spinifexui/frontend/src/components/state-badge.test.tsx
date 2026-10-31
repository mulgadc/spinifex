import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { StateBadge } from "./state-badge"

describe("StateBadge", () => {
  it.each(["running", "available", "completed", "Active"])(
    "renders success for success state '%s'",
    (state) => {
      render(<StateBadge state={state} />)
      const badge = screen.getByText(state)
      expect(badge).toHaveAttribute("data-tone", "success")
    },
  )

  it.each(["stopped", "error"])(
    "renders danger for error state '%s'",
    (state) => {
      render(<StateBadge state={state} />)
      const badge = screen.getByText(state)
      expect(badge).toHaveAttribute("data-tone", "danger")
    },
  )

  it.each(["pending", "shutting-down", "stopping"])(
    "renders warning for warning state '%s'",
    (state) => {
      render(<StateBadge state={state} />)
      const badge = screen.getByText(state)
      expect(badge).toHaveAttribute("data-tone", "warning")
    },
  )

  // Every GuardrailStatus, KnowledgeBaseStatus, DataSourceStatus and
  // IngestionJobStatus value except STOPPED, which stays neutral.
  it.each(["ACTIVE", "AVAILABLE", "COMPLETE", "READY"])(
    "renders success for Bedrock success status '%s'",
    (state) => {
      render(<StateBadge state={state} />)
      expect(screen.getByText(state)).toHaveAttribute("data-tone", "success")
    },
  )

  it.each(["FAILED", "DELETE_UNSUCCESSFUL", "UPDATE_UNSUCCESSFUL"])(
    "renders danger for Bedrock error status '%s'",
    (state) => {
      render(<StateBadge state={state} />)
      expect(screen.getByText(state)).toHaveAttribute("data-tone", "danger")
    },
  )

  it.each([
    "CREATING",
    "DELETING",
    "IN_PROGRESS",
    "STARTING",
    "STOPPING",
    "UPDATING",
    "VERSIONING",
  ])("renders warning for Bedrock in-progress status '%s'", (state) => {
    render(<StateBadge state={state} />)
    expect(screen.getByText(state)).toHaveAttribute("data-tone", "warning")
  })

  it.each([
    ["healthy", "success"],
    ["unhealthy", "danger"],
    ["unavailable", "danger"],
    ["initial", "warning"],
    ["draining", "warning"],
  ])("renders ELBv2 target health '%s' as %s", (state, tone) => {
    render(<StateBadge state={state} />)
    expect(screen.getByText(state)).toHaveAttribute("data-tone", tone)
  })

  it("renders muted for terminated state", () => {
    render(<StateBadge state="terminated" />)
    const badge = screen.getByText("terminated")
    expect(badge).toHaveAttribute("data-tone", "muted")
  })

  it("renders neutral for unknown state", () => {
    render(<StateBadge state="something-else" />)
    const badge = screen.getByText("something-else")
    expect(badge).toHaveAttribute("data-tone", "neutral")
  })

  it("renders neutral for undefined state", () => {
    const { container } = render(<StateBadge state={undefined} />)
    const badge = container.firstElementChild
    expect(badge).toHaveAttribute("data-tone", "neutral")
  })
})
