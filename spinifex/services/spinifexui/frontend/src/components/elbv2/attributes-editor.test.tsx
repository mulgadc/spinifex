import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import {
  albAttributeSpecs,
  AttributesEditor,
  type AttributeSpec,
  targetGroupAttributeSpecs,
} from "./attributes-editor"

const SIMPLE_SPECS: AttributeSpec[] = [
  {
    key: "deletion_protection.enabled",
    label: "Deletion protection",
    type: "bool",
  },
  {
    key: "idle_timeout.timeout_seconds",
    label: "Idle timeout (s)",
    type: "int",
    min: 1,
    max: 4000,
  },
  { key: "access_logs.s3.bucket", label: "Bucket", type: "text" },
  {
    key: "routing.http.desync_mitigation_mode",
    label: "Desync mitigation",
    type: "select",
    options: ["defensive", "strictest", "monitor"] as const,
  },
]

const INITIAL = [
  { Key: "deletion_protection.enabled", Value: "false" },
  { Key: "idle_timeout.timeout_seconds", Value: "60" },
  { Key: "access_logs.s3.bucket", Value: "" },
  { Key: "routing.http.desync_mitigation_mode", Value: "defensive" },
]

describe("AttributesEditor", () => {
  it("seeds fields from initial attributes and disables Save with no changes", () => {
    render(
      <AttributesEditor
        attributes={INITIAL}
        isPending={false}
        onSubmit={() => {}}
        specs={SIMPLE_SPECS}
      />,
    )

    expect(screen.getByLabelText("Deletion protection")).not.toBeChecked()
    expect(screen.getByLabelText("Idle timeout (s)")).toHaveValue("60")
    expect(screen.getByText("No changes")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
  })

  it("submits only the keys the user changed", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(
      <AttributesEditor
        attributes={INITIAL}
        isPending={false}
        onSubmit={onSubmit}
        specs={SIMPLE_SPECS}
      />,
    )

    await user.click(screen.getByLabelText("Deletion protection"))
    const idle = screen.getByLabelText("Idle timeout (s)")
    await user.clear(idle)
    await user.type(idle, "120")

    await user.click(screen.getByRole("button", { name: /save changes/i }))

    expect(onSubmit).toHaveBeenCalledWith([
      { key: "deletion_protection.enabled", value: "true" },
      { key: "idle_timeout.timeout_seconds", value: "120" },
    ])
  })

  it("blocks submit when an int field is out of range", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(
      <AttributesEditor
        attributes={INITIAL}
        isPending={false}
        onSubmit={onSubmit}
        specs={SIMPLE_SPECS}
      />,
    )

    const idle = screen.getByLabelText("Idle timeout (s)")
    await user.clear(idle)
    await user.type(idle, "9999")

    const save = screen.getByRole("button", { name: /save changes/i })
    expect(save).toBeDisabled()
    expect(screen.getByText(/at most 4000/i)).toBeInTheDocument()
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it("Reset reverts dirty values back to initial", async () => {
    const user = userEvent.setup()
    render(
      <AttributesEditor
        attributes={INITIAL}
        isPending={false}
        onSubmit={() => {}}
        specs={SIMPLE_SPECS}
      />,
    )

    await user.click(screen.getByLabelText("Deletion protection"))
    expect(screen.getByLabelText("Deletion protection")).toBeChecked()

    await user.click(screen.getByRole("button", { name: /reset/i }))

    expect(screen.getByLabelText("Deletion protection")).not.toBeChecked()
    expect(screen.getByText("No changes")).toBeInTheDocument()
  })

  it("renders a Save button in pending state", () => {
    render(
      <AttributesEditor
        attributes={INITIAL}
        isPending={true}
        onSubmit={() => {}}
        specs={SIMPLE_SPECS}
      />,
    )
    expect(screen.getByRole("button", { name: /saving/i })).toBeDisabled()
  })

  it("renders an error banner when the mutation fails", () => {
    render(
      <AttributesEditor
        attributes={INITIAL}
        error={new Error("boom")}
        isPending={false}
        onSubmit={() => {}}
        specs={SIMPLE_SPECS}
      />,
    )
    expect(screen.getByRole("alert")).toHaveTextContent("boom")
  })

  it("exports spec sets covering every DefaultLoadBalancerAttributes/TG key required by plan", () => {
    const lbKeys = albAttributeSpecs.map((s) => s.key)
    expect(lbKeys).toContain("deletion_protection.enabled")
    expect(lbKeys).toContain("load_balancing.cross_zone.enabled")
    expect(lbKeys).toContain("idle_timeout.timeout_seconds")
    expect(lbKeys).toContain("access_logs.s3.bucket")
    expect(lbKeys).toContain("connection_logs.s3.prefix")

    const tgKeys = targetGroupAttributeSpecs.map((s) => s.key)
    expect(tgKeys).toStrictEqual([
      "deregistration_delay.timeout_seconds",
      "stickiness.enabled",
      "stickiness.type",
      "stickiness.lb_cookie.duration_seconds",
      "load_balancing.cross_zone.enabled",
      "load_balancing.algorithm.type",
      "slow_start.duration_seconds",
    ])
  })
})
