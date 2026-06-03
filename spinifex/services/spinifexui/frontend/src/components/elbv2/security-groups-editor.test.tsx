import type { SecurityGroup } from "@aws-sdk/client-ec2"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { SecurityGroupsEditor } from "./security-groups-editor"

const AVAILABLE: SecurityGroup[] = [
  { GroupId: "sg-1", GroupName: "default", VpcId: "vpc-a" },
  { GroupId: "sg-2", GroupName: "web", VpcId: "vpc-a" },
  { GroupId: "sg-3", GroupName: "db", VpcId: "vpc-a" },
]

describe("SecurityGroupsEditor", () => {
  it("seeds selection from current and disables Save with no changes", () => {
    render(
      <SecurityGroupsEditor
        available={AVAILABLE}
        current={["sg-1"]}
        isPending={false}
        onSubmit={() => {}}
      />,
    )

    expect(screen.getByLabelText("Security group sg-1 (default)")).toBeChecked()
    expect(screen.getByLabelText("Security group sg-2 (web)")).not.toBeChecked()
    expect(screen.getByText("No changes")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
  })

  it("submits the new selection set", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(
      <SecurityGroupsEditor
        available={AVAILABLE}
        current={["sg-1"]}
        isPending={false}
        onSubmit={onSubmit}
      />,
    )

    await user.click(screen.getByLabelText("Security group sg-2 (web)"))
    await user.click(screen.getByRole("button", { name: /save changes/i }))

    expect(onSubmit).toHaveBeenCalledWith(["sg-1", "sg-2"])
  })

  it("blocks submit when no security group is selected", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(
      <SecurityGroupsEditor
        available={AVAILABLE}
        current={["sg-1"]}
        isPending={false}
        onSubmit={onSubmit}
      />,
    )

    await user.click(screen.getByLabelText("Security group sg-1 (default)"))

    expect(screen.getByText(/at least one security group/i)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it("Reset reverts selection back to initial", async () => {
    const user = userEvent.setup()
    render(
      <SecurityGroupsEditor
        available={AVAILABLE}
        current={["sg-1"]}
        isPending={false}
        onSubmit={() => {}}
      />,
    )

    await user.click(screen.getByLabelText("Security group sg-2 (web)"))
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: /reset/i }))

    expect(screen.getByLabelText("Security group sg-2 (web)")).not.toBeChecked()
    expect(screen.getByText("No changes")).toBeInTheDocument()
  })

  it("shows an empty state when the VPC has no security groups", () => {
    render(
      <SecurityGroupsEditor
        available={[]}
        current={[]}
        isPending={false}
        onSubmit={() => {}}
      />,
    )
    expect(
      screen.getByText("No security groups in this VPC."),
    ).toBeInTheDocument()
  })

  it("renders an error banner when the mutation fails", () => {
    render(
      <SecurityGroupsEditor
        available={AVAILABLE}
        current={["sg-1"]}
        error={new Error("boom")}
        isPending={false}
        onSubmit={() => {}}
      />,
    )
    expect(screen.getByRole("alert")).toHaveTextContent("boom")
  })
})
