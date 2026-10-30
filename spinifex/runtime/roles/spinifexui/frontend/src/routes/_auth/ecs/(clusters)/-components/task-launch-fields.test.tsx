import { fireEvent, render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { AwsvpcNetworkFields } from "./task-launch-fields"

const subnets = [
  {
    SubnetId: "subnet-a",
    CidrBlock: "10.0.1.0/24",
    AvailabilityZone: "ap-southeast-2a",
  },
  { SubnetId: "subnet-b", CidrBlock: "10.0.2.0/24" },
]

const securityGroups = [{ GroupId: "sg-1", GroupName: "web" }]

function renderFields(selectedSubnets: string[], assignPublicIp = false) {
  const onSubnetsChange = vi.fn()
  const onSecurityGroupsChange = vi.fn()
  const onAssignPublicIpChange = vi.fn()

  render(
    <AwsvpcNetworkFields
      assignPublicIp={assignPublicIp}
      onAssignPublicIpChange={onAssignPublicIpChange}
      onSecurityGroupsChange={onSecurityGroupsChange}
      onSubnetsChange={onSubnetsChange}
      securityGroups={securityGroups}
      selectedSecurityGroups={[]}
      selectedSubnets={selectedSubnets}
      subnets={subnets}
    />,
  )

  return { onAssignPublicIpChange, onSecurityGroupsChange, onSubnetsChange }
}

describe("AwsvpcNetworkFields", () => {
  it("adds an unselected subnet and removes a selected one, by switch", () => {
    const { onSubnetsChange } = renderFields(["subnet-a"])

    expect(
      screen.getByRole("switch", { name: "Subnet subnet-a" }),
    ).toBeChecked()

    fireEvent.click(screen.getByRole("switch", { name: "Subnet subnet-b" }))
    expect(onSubnetsChange).toHaveBeenLastCalledWith(["subnet-a", "subnet-b"])

    fireEvent.click(screen.getByRole("switch", { name: "Subnet subnet-a" }))
    expect(onSubnetsChange).toHaveBeenLastCalledWith([])
  })

  it("names each subnet's zone and CIDR beside it", () => {
    renderFields([])

    expect(
      screen.getByText("ap-southeast-2a · 10.0.1.0/24"),
    ).toBeInTheDocument()
  })

  it("reports the security group selection", () => {
    const { onSecurityGroupsChange } = renderFields([])

    fireEvent.click(screen.getByLabelText("Security group sg-1 (web)"))
    expect(onSecurityGroupsChange).toHaveBeenCalledWith(["sg-1"])
  })

  it("sets the public IP with a switch, not a checkbox", () => {
    const { onAssignPublicIpChange } = renderFields([], true)
    const toggle = screen.getByRole("switch", { name: "Assign public IP" })

    expect(toggle).toBeChecked()
    fireEvent.click(toggle)
    expect(onAssignPublicIpChange).toHaveBeenCalledWith(
      false,
      expect.anything(),
    )
  })
})
