import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/awsClient", () => ({
  getEcsClient: () => ({ send: vi.fn() }),
  getEc2Client: () => ({ send: vi.fn() }),
}))

const { provisionCapacity } = vi.hoisted(() => ({
  provisionCapacity: vi.fn(),
}))

vi.mock("@/lib/ecs-provision", () => ({
  provisionCapacity,
}))

import { ProvisionCapacityDialog } from "./provision-capacity-dialog"

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "subnets"], {
    Subnets: [
      {
        SubnetId: "subnet-1",
        CidrBlock: "10.0.1.0/24",
        AvailabilityZone: "ap-southeast-2a",
      },
    ],
  })
  qc.setQueryData(["ec2", "securityGroups"], {
    SecurityGroups: [{ GroupId: "sg-1", GroupName: "default" }],
  })
  qc.setQueryData(["ec2", "keypairs"], {
    KeyPairs: [{ KeyPairId: "key-1", KeyName: "my-key" }],
  })
  return qc
}

describe("ProvisionCapacityDialog", () => {
  it("renders the capacity fields", () => {
    renderWithClient(
      <ProvisionCapacityDialog clusterName="web" onOpenChange={vi.fn()} open />,
      seed(),
    )
    expect(screen.getByLabelText("Instance type")).toHaveValue("t3.small")
    expect(screen.getByLabelText("Count")).toHaveValue(1)
    expect(screen.getByLabelText("Subnet")).toBeInTheDocument()
    expect(screen.getByLabelText("Security group")).toBeInTheDocument()
    expect(screen.getByLabelText("Key pair")).toBeInTheDocument()
  })

  it("provisions with the selected values", async () => {
    provisionCapacity.mockResolvedValue({ InstanceIDs: ["i-123"] })
    const onOpenChange = vi.fn()
    renderWithClient(
      <ProvisionCapacityDialog
        clusterName="web"
        onOpenChange={onOpenChange}
        open
      />,
      seed(),
    )

    fireEvent.change(screen.getByLabelText("Subnet"), {
      target: { value: "subnet-1" },
    })
    fireEvent.change(screen.getByLabelText("Security group"), {
      target: { value: "sg-1" },
    })
    fireEvent.change(screen.getByLabelText("Key pair"), {
      target: { value: "my-key" },
    })

    fireEvent.click(screen.getByRole("button", { name: "Provision" }))

    await waitFor(() => {
      expect(provisionCapacity).toHaveBeenCalledWith({
        Cluster: "web",
        InstanceType: "t3.small",
        Count: 1,
        SubnetID: "subnet-1",
        SecurityGroupID: "sg-1",
        KeyName: "my-key",
      })
    })
    await waitFor(() => {
      expect(onOpenChange).toHaveBeenCalledWith(false)
    })
  })

  it("disables submit until required fields are selected", () => {
    renderWithClient(
      <ProvisionCapacityDialog clusterName="web" onOpenChange={vi.fn()} open />,
      seed(),
    )
    expect(screen.getByRole("button", { name: "Provision" })).toBeDisabled()
  })
})
