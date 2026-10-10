import type { Vpc } from "@aws-sdk/client-ec2"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { useForm } from "react-hook-form"
import { describe, expect, it } from "vitest"

import type { CreateTargetGroupFormData } from "@/types/elbv2"

import { TargetGroupForm } from "./target-group-form"

const VPCS: Vpc[] = [
  { VpcId: "vpc-aaa", CidrBlock: "10.0.0.0/16", Tags: [] },
  { VpcId: "vpc-bbb", CidrBlock: "10.1.0.0/16", Tags: [] },
]

function Harness({
  onFormRef,
}: {
  onFormRef?: (
    form: ReturnType<typeof useForm<CreateTargetGroupFormData>>,
  ) => void
}) {
  const form = useForm<CreateTargetGroupFormData>({
    defaultValues: {
      name: "",
      protocol: "HTTP",
      port: 80,
      vpcId: "vpc-aaa",
      healthCheck: {
        protocol: "HTTP",
        path: "/",
        port: "traffic-port",
        intervalSeconds: 30,
        timeoutSeconds: 5,
        healthyThresholdCount: 5,
        unhealthyThresholdCount: 2,
        matcher: "200",
      },
      tags: [],
    },
  })
  onFormRef?.(form)
  return <TargetGroupForm form={form} vpcs={VPCS} />
}

describe("TargetGroupForm", () => {
  it("renders the visible field inputs", () => {
    render(<Harness />)
    expect(screen.getByLabelText("Name")).toBeInTheDocument()
    expect(screen.getByLabelText("Port")).toBeInTheDocument()
    expect(screen.getByText("Health check settings")).toBeInTheDocument()
    expect(screen.getByText("Tags")).toBeInTheDocument()
  })

  it("adds tag rows via the add-tag button", async () => {
    const user = userEvent.setup()
    let form: ReturnType<typeof useForm<CreateTargetGroupFormData>> | undefined
    render(<Harness onFormRef={(f) => (form = f)} />)

    await user.click(screen.getByRole("button", { name: /add tag/i }))
    expect(form?.getValues("tags")).toHaveLength(1)
    await user.click(screen.getByRole("button", { name: /add tag/i }))
    expect(form?.getValues("tags")).toHaveLength(2)
  })
})
