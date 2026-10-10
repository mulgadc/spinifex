import type { Subnet } from "@aws-sdk/client-ec2"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { SubnetsEditor } from "./subnets-editor"

const AVAILABLE: Subnet[] = [
  {
    SubnetId: "subnet-1",
    VpcId: "vpc-a",
    AvailabilityZone: "az-1",
    CidrBlock: "10.0.1.0/24",
  },
  {
    SubnetId: "subnet-2",
    VpcId: "vpc-a",
    AvailabilityZone: "az-2",
    CidrBlock: "10.0.2.0/24",
  },
  {
    SubnetId: "subnet-3",
    VpcId: "vpc-a",
    AvailabilityZone: "az-3",
    CidrBlock: "10.0.3.0/24",
  },
]

describe("SubnetsEditor", () => {
  it("always warns about the relaunch data-plane interruption", () => {
    render(
      <SubnetsEditor
        available={AVAILABLE}
        current={["subnet-1"]}
        isPending={false}
        onSubmit={() => {}}
      />,
    )
    expect(screen.getByRole("note")).toHaveTextContent(/relaunch/i)
  })

  it("seeds selection from current and disables Save with no changes", () => {
    render(
      <SubnetsEditor
        available={AVAILABLE}
        current={["subnet-1"]}
        isPending={false}
        onSubmit={() => {}}
      />,
    )

    expect(screen.getByLabelText("Subnet subnet-1")).toBeChecked()
    expect(screen.getByLabelText("Subnet subnet-2")).not.toBeChecked()
    expect(screen.getByText("No changes")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
  })

  it("submits the new selection set", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(
      <SubnetsEditor
        available={AVAILABLE}
        current={["subnet-1"]}
        isPending={false}
        onSubmit={onSubmit}
      />,
    )

    await user.click(screen.getByLabelText("Subnet subnet-2"))
    await user.click(screen.getByRole("button", { name: /save changes/i }))

    expect(onSubmit).toHaveBeenCalledWith(["subnet-1", "subnet-2"])
  })

  it("blocks submit when no subnet is selected", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(
      <SubnetsEditor
        available={AVAILABLE}
        current={["subnet-1"]}
        isPending={false}
        onSubmit={onSubmit}
      />,
    )

    await user.click(screen.getByLabelText("Subnet subnet-1"))

    expect(screen.getByText(/at least one subnet/i)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it("Reset reverts selection back to initial", async () => {
    const user = userEvent.setup()
    render(
      <SubnetsEditor
        available={AVAILABLE}
        current={["subnet-1"]}
        isPending={false}
        onSubmit={() => {}}
      />,
    )

    await user.click(screen.getByLabelText("Subnet subnet-2"))
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: /reset/i }))

    expect(screen.getByLabelText("Subnet subnet-2")).not.toBeChecked()
    expect(screen.getByText("No changes")).toBeInTheDocument()
  })

  it("shows an empty state when the VPC has no subnets", () => {
    render(
      <SubnetsEditor
        available={[]}
        current={[]}
        isPending={false}
        onSubmit={() => {}}
      />,
    )
    expect(screen.getByText("No subnets in this VPC.")).toBeInTheDocument()
  })

  it("renders an error banner when the mutation fails", () => {
    render(
      <SubnetsEditor
        available={AVAILABLE}
        current={["subnet-1"]}
        error={new Error("boom")}
        isPending={false}
        onSubmit={() => {}}
      />,
    )
    expect(screen.getByRole("alert")).toHaveTextContent("boom")
  })
})
