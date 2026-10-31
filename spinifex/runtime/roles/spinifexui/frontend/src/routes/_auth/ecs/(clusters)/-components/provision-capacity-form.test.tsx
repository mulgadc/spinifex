import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn())
})

const { provisionCapacity } = vi.hoisted(() => ({
  provisionCapacity: vi.fn(),
}))

vi.mock("@/lib/ecs-provision", () => ({
  provisionCapacity,
}))

import { ProvisionCapacityForm } from "./provision-capacity-form"

async function choose(
  user: ReturnType<typeof userEvent.setup>,
  label: string,
  option: string | RegExp,
) {
  await user.click(screen.getByLabelText(label))
  await user.click(screen.getByRole("option", { name: option }))
}

const ECS_NODE_IMAGE = {
  ImageId: "ami-ecs",
  Tags: [{ Key: "spinifex:managed-by", Value: "ecs" }],
}

function seed(images = [ECS_NODE_IMAGE]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "images"], { Images: images })
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
  qc.setQueryData(["ec2", "instances", "types"], {
    InstanceTypes: [
      { InstanceType: "t3.small" },
      {
        InstanceType: "mig.1g.24gb",
        GpuInfo: {
          Gpus: [
            {
              Name: "RTX Pro 6000 Blackwell SE",
              Manufacturer: "NVIDIA",
              Count: 1,
              MemoryInfo: { SizeInMiB: 24_576 },
            },
          ],
          TotalGpuMemoryInMiB: 24_576,
        },
      },
    ],
  })

  return qc
}

describe("ProvisionCapacityForm", () => {
  it("renders the capacity fields", () => {
    renderWithClient(
      <ProvisionCapacityForm clusterName="web" onClose={vi.fn()} />,
      seed(),
    )
    expect(screen.getByLabelText("Instance type")).toHaveTextContent("t3.small")
    expect(screen.getByLabelText("Count")).toHaveValue(1)
    expect(screen.getByLabelText("Subnet")).toBeInTheDocument()
    expect(screen.getByLabelText("Security group")).toBeInTheDocument()
    expect(screen.getByLabelText("Key pair")).toBeInTheDocument()
  })

  it("provisions with the selected values", async () => {
    provisionCapacity.mockResolvedValue({ InstanceIDs: ["i-123"] })
    const user = userEvent.setup()
    const onClose = vi.fn()
    renderWithClient(
      <ProvisionCapacityForm clusterName="web" onClose={onClose} />,
      seed(),
    )

    await choose(user, "Subnet", /subnet-1/)
    await choose(user, "Security group", /sg-1/)
    await choose(user, "Key pair", "my-key")
    expect(screen.getByLabelText("Subnet")).toHaveTextContent("subnet-1")

    await user.click(screen.getByRole("button", { name: "Provision" }))

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
      expect(onClose).toHaveBeenCalled()
    })
  })

  it("asks for the ECS node image in place of the fields when it is missing", () => {
    renderWithClient(
      <ProvisionCapacityForm clusterName="web" onClose={vi.fn()} />,
      seed([]),
    )
    expect(screen.getByText("ECS system image not found")).toBeInTheDocument()
    expect(screen.queryByLabelText("Subnet")).not.toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Provision" })).toBeDisabled()
  })

  it("disables submit until required fields are selected", () => {
    renderWithClient(
      <ProvisionCapacityForm clusterName="web" onClose={vi.fn()} />,
      seed(),
    )
    expect(screen.getByRole("button", { name: "Provision" })).toBeDisabled()
  })

  it("uses the shared GPU instance-type picker, grouping GPU types", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <ProvisionCapacityForm clusterName="web" onClose={vi.fn()} />,
      seed(),
    )

    await user.click(screen.getByLabelText("Instance type"))

    expect(screen.getByText("Standard")).toBeInTheDocument()
    expect(screen.getByText("GPU-Accelerated")).toBeInTheDocument()

    await user.click(screen.getByRole("option", { name: /mig\.1g\.24gb/ }))

    expect(screen.getByLabelText("Instance type")).toHaveTextContent(
      "mig.1g.24gb",
    )
  })

  it("provisions with the selected GPU instance type", async () => {
    provisionCapacity.mockResolvedValue({ InstanceIDs: ["i-123"] })
    const user = userEvent.setup()
    const onClose = vi.fn()
    renderWithClient(
      <ProvisionCapacityForm clusterName="web" onClose={onClose} />,
      seed(),
    )

    await user.click(screen.getByLabelText("Instance type"))
    await user.click(screen.getByRole("option", { name: /mig\.1g\.24gb/ }))

    await choose(user, "Subnet", /subnet-1/)
    await choose(user, "Security group", /sg-1/)

    await user.click(screen.getByRole("button", { name: "Provision" }))

    await waitFor(() => {
      expect(provisionCapacity).toHaveBeenCalledWith(
        expect.objectContaining({ InstanceType: "mig.1g.24gb" }),
      )
    })
  })
})
