import type { Instance } from "@aws-sdk/client-ec2"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { RegisterTargetsForm } from "./register-targets-form"

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
    targetGroupArn: "arn:tg",
    onClose: vi.fn(),
    instances: INSTANCES,
    defaultPort: 80,
    isPending: false,
    error: null,
    onSubmit: vi.fn(async () => {}),
  }
}

describe("RegisterTargetsForm", () => {
  it("lists instance ids and name tags", () => {
    render(<RegisterTargetsForm {...defaultProps()} />)
    expect(screen.getByText("i-aaa")).toBeInTheDocument()
    expect(screen.getByText("(web-1) running")).toBeInTheDocument()
    expect(screen.getByText("i-bbb")).toBeInTheDocument()
  })

  it("shows an empty message when there are no instances", () => {
    render(<RegisterTargetsForm {...defaultProps()} instances={[]} />)
    expect(
      screen.getByText("No instances available in this VPC."),
    ).toBeInTheDocument()
  })

  it("requires at least one selection before submitting", async () => {
    const onSubmit = vi.fn(async () => {})
    const user = userEvent.setup()
    render(<RegisterTargetsForm {...defaultProps()} onSubmit={onSubmit} />)
    await user.click(screen.getByRole("button", { name: "Register" }))
    expect(onSubmit).not.toHaveBeenCalled()
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Select at least one instance.",
    )
  })

  it("submits selected targets with no port override", async () => {
    const onSubmit = vi.fn(async () => {})
    const user = userEvent.setup()
    render(<RegisterTargetsForm {...defaultProps()} onSubmit={onSubmit} />)
    await user.click(
      screen.getByRole("switch", { name: "Register target i-aaa" }),
    )
    await user.click(screen.getByRole("button", { name: "Register" }))
    expect(onSubmit).toHaveBeenCalledWith([{ id: "i-aaa" }])
  })

  it("passes a valid port override through", async () => {
    const onSubmit = vi.fn(async () => {})
    const user = userEvent.setup()
    render(<RegisterTargetsForm {...defaultProps()} onSubmit={onSubmit} />)
    await user.click(
      screen.getByRole("switch", { name: "Register target i-aaa" }),
    )
    await user.type(screen.getByLabelText("Port override for i-aaa"), "8080")
    await user.click(screen.getByRole("button", { name: "Register" }))
    expect(onSubmit).toHaveBeenCalledWith([{ id: "i-aaa", port: 8080 }])
  })

  it("rejects an invalid port override", async () => {
    const onSubmit = vi.fn(async () => {})
    const user = userEvent.setup()
    render(<RegisterTargetsForm {...defaultProps()} onSubmit={onSubmit} />)
    await user.click(
      screen.getByRole("switch", { name: "Register target i-aaa" }),
    )
    await user.type(screen.getByLabelText("Port override for i-aaa"), "99999")
    await user.click(screen.getByRole("button", { name: "Register" }))
    expect(onSubmit).not.toHaveBeenCalled()
    expect(screen.getByRole("alert")).toHaveTextContent("Port for i-aaa")
  })

  it("shows the selected targets in the CLI command", async () => {
    const user = userEvent.setup()
    render(<RegisterTargetsForm {...defaultProps()} />)
    await user.click(
      screen.getByRole("switch", { name: "Register target i-aaa" }),
    )
    await user.type(screen.getByLabelText("Port override for i-aaa"), "8080")
    await user.click(screen.getByRole("button", { name: "AWS CLI" }))
    expect(screen.getByText(/Id=i-aaa,Port=8080/)).toBeInTheDocument()
  })

  it("shows pending label while submitting", () => {
    render(<RegisterTargetsForm {...defaultProps()} isPending={true} />)
    expect(screen.getByText("Registering\u2026")).toBeInTheDocument()
  })
})
