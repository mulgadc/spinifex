import type { Instance } from "@aws-sdk/client-ec2"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { RegisterTargetsDialog } from "./register-targets-dialog"

const INSTANCES: Instance[] = [
  {
    InstanceId: "i-aaa",
    State: { Name: "running" },
    Tags: [{ Key: "Name", Value: "web-1" }],
  },
  {
    InstanceId: "i-bbb",
    State: { Name: "stopped" },
    Tags: [],
  },
]

function defaultProps() {
  return {
    open: true,
    onOpenChange: vi.fn(),
    instances: INSTANCES,
    defaultPort: 80,
    isPending: false,
    onConfirm: vi.fn(),
  }
}

describe("RegisterTargetsDialog", () => {
  it("lists instance ids and name tags", () => {
    render(<RegisterTargetsDialog {...defaultProps()} />)
    expect(screen.getByText("i-aaa")).toBeInTheDocument()
    expect(screen.getByText("(web-1)")).toBeInTheDocument()
    expect(screen.getByText("i-bbb")).toBeInTheDocument()
  })

  it("shows an empty message when there are no instances", () => {
    render(<RegisterTargetsDialog {...defaultProps()} instances={[]} />)
    expect(
      screen.getByText("No instances available in this VPC."),
    ).toBeInTheDocument()
  })

  it("requires at least one selection before submitting", async () => {
    const onConfirm = vi.fn()
    const user = userEvent.setup()
    render(<RegisterTargetsDialog {...defaultProps()} onConfirm={onConfirm} />)
    await user.click(screen.getByRole("button", { name: "Register" }))
    expect(onConfirm).not.toHaveBeenCalled()
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Select at least one instance.",
    )
  })

  it("submits selected targets with no port override", async () => {
    const onConfirm = vi.fn()
    const user = userEvent.setup()
    render(<RegisterTargetsDialog {...defaultProps()} onConfirm={onConfirm} />)
    await user.click(screen.getByLabelText(/i-aaa/))
    await user.click(screen.getByRole("button", { name: "Register" }))
    expect(onConfirm).toHaveBeenCalledWith([{ id: "i-aaa" }])
  })

  it("passes a valid port override through", async () => {
    const onConfirm = vi.fn()
    const user = userEvent.setup()
    render(<RegisterTargetsDialog {...defaultProps()} onConfirm={onConfirm} />)
    await user.click(screen.getByLabelText(/i-aaa/))
    await user.type(screen.getByLabelText("Port override for i-aaa"), "8080")
    await user.click(screen.getByRole("button", { name: "Register" }))
    expect(onConfirm).toHaveBeenCalledWith([{ id: "i-aaa", port: 8080 }])
  })

  it("rejects an invalid port override", async () => {
    const onConfirm = vi.fn()
    const user = userEvent.setup()
    render(<RegisterTargetsDialog {...defaultProps()} onConfirm={onConfirm} />)
    await user.click(screen.getByLabelText(/i-aaa/))
    await user.type(screen.getByLabelText("Port override for i-aaa"), "99999")
    await user.click(screen.getByRole("button", { name: "Register" }))
    expect(onConfirm).not.toHaveBeenCalled()
    expect(screen.getByRole("alert")).toHaveTextContent("Port for i-aaa")
  })

  it("shows pending label while submitting", () => {
    render(<RegisterTargetsDialog {...defaultProps()} isPending={true} />)
    expect(screen.getByText("Registering\u2026")).toBeInTheDocument()
  })
})
