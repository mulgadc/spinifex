import { fireEvent, render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { AwsvpcNetworkFields } from "./task-launch-fields"

const subnets = [
  { SubnetId: "subnet-a", CidrBlock: "10.0.1.0/24" },
  { SubnetId: "subnet-b", CidrBlock: "10.0.2.0/24" },
]

const securityGroups = [{ GroupId: "sg-1", GroupName: "web" }]

function renderFields(selectedSubnets: string[]) {
  const onSubnetsChange = vi.fn()
  const onSecurityGroupsChange = vi.fn()

  render(
    <AwsvpcNetworkFields
      assignPublicIp={{
        name: "assignPublicIp",
        onBlur: vi.fn(),
        onChange: vi.fn(),
        ref: vi.fn(),
      }}
      onSecurityGroupsChange={onSecurityGroupsChange}
      onSubnetsChange={onSubnetsChange}
      securityGroups={securityGroups}
      selectedSecurityGroups={[]}
      selectedSubnets={selectedSubnets}
      subnets={subnets}
    />,
  )

  return { onSecurityGroupsChange, onSubnetsChange }
}

describe("AwsvpcNetworkFields", () => {
  it("adds an unchecked subnet and removes a checked one", () => {
    const { onSubnetsChange } = renderFields(["subnet-a"])

    fireEvent.click(screen.getByRole("checkbox", { name: /subnet-b/ }))
    expect(onSubnetsChange).toHaveBeenLastCalledWith(["subnet-a", "subnet-b"])

    fireEvent.click(screen.getByRole("checkbox", { name: /subnet-a/ }))
    expect(onSubnetsChange).toHaveBeenLastCalledWith([])
  })

  it("reports the security group selection", () => {
    const { onSecurityGroupsChange } = renderFields([])

    fireEvent.click(screen.getByLabelText("Security group sg-1 (web)"))
    expect(onSecurityGroupsChange).toHaveBeenCalledWith(["sg-1"])
  })
})
