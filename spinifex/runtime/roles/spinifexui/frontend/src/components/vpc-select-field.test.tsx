import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { useForm } from "react-hook-form"
import { describe, expect, it, vi } from "vitest"

import { VpcSelectField } from "./vpc-select-field"

const vpcs = [
  {
    VpcId: "vpc-named",
    CidrBlock: "10.0.0.0/16",
    Tags: [{ Key: "Name", Value: "prod" }],
  },
  { VpcId: "vpc-bare", CidrBlock: "10.1.0.0/16" },
]

function TestForm({ onVpcChange }: { onVpcChange: (value: string) => void }) {
  const { control } = useForm<{ vpcId: string }>({
    defaultValues: { vpcId: "" },
  })

  return (
    <VpcSelectField
      control={control}
      id="vpc"
      name="vpcId"
      onValueChange={onVpcChange}
      vpcs={vpcs}
    />
  )
}

describe("VpcSelectField", () => {
  it("offers each VPC by name or CIDR and reports the chosen id", async () => {
    const user = userEvent.setup()
    const onVpcChange = vi.fn()
    render(<TestForm onVpcChange={onVpcChange} />)

    await user.click(screen.getByLabelText("VPC"))

    await expect(
      screen.findByRole("option", { name: "vpc-named (prod)" }),
    ).resolves.toBeInTheDocument()

    await user.click(
      screen.getByRole("option", { name: "vpc-bare (10.1.0.0/16)" }),
    )

    expect(onVpcChange).toHaveBeenCalledWith("vpc-bare")
  })
})
