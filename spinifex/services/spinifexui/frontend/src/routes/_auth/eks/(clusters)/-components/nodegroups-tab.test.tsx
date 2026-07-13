import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { fireEvent, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/awsClient", () => ({
  getEksClient: () => ({ send: vi.fn() }),
  getEc2Client: () => ({ send: vi.fn() }),
  getIamClient: () => ({ send: vi.fn() }),
}))

import { NodegroupsTab } from "./nodegroups-tab"

const CLUSTER = "demo"

const INSTANCE_TYPES = [
  { InstanceType: "t3.medium" },
  {
    InstanceType: "g5.xlarge",
    GpuInfo: {
      Gpus: [
        {
          Name: "A10G",
          Manufacturer: "NVIDIA",
          Count: 1,
          MemoryInfo: { SizeInMiB: 24_576 },
        },
      ],
      TotalGpuMemoryInMiB: 24_576,
    },
  },
] as InstanceTypeInfo[]

function seed(opts?: { nodegroupVersion?: string; gpuNodegroup?: boolean }) {
  const qc = createTestQueryClient()
  qc.setQueryData(["eks", "clusters", CLUSTER, "nodegroups"], {
    nodegroups: ["ng-1"],
  })
  qc.setQueryData(["eks", "clusters", CLUSTER, "nodegroups", "ng-1"], {
    nodegroup: {
      status: "ACTIVE",
      version: opts?.nodegroupVersion ?? "1.29",
      instanceTypes: [opts?.gpuNodegroup ? "g5.xlarge" : "t3.medium"],
      amiType: opts?.gpuNodegroup ? "AL2_x86_64_GPU" : "AL2_x86_64",
      capacityType: "ON_DEMAND",
      scalingConfig: { minSize: 1, desiredSize: 2, maxSize: 3 },
    },
  })
  qc.setQueryData(["iam", "roles"], {
    Roles: [{ Arn: "arn:aws:iam::0:role/node", RoleName: "node" }],
  })
  qc.setQueryData(["ec2", "subnets"], {
    Subnets: [
      { SubnetId: "subnet-a", CidrBlock: "10.0.1.0/24", VpcId: "vpc-1" },
    ],
  })
  qc.setQueryData(["ec2", "instances", "types"], {
    InstanceTypes: INSTANCE_TYPES,
  })
  return qc
}

describe("NodegroupsTab", () => {
  it("renders node group details", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    expect(screen.getByText("ng-1")).toBeInTheDocument()
    expect(screen.getByText("ACTIVE")).toBeInTheDocument()
    expect(screen.getByText("1 / 2 / 3")).toBeInTheDocument()
  })

  it("shows empty state when there are no node groups", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["eks", "clusters", CLUSTER, "nodegroups"], {
      nodegroups: [],
    })
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      qc,
    )
    expect(screen.getByText("No node groups.")).toBeInTheDocument()
  })

  it("opens the add node group dialog", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "Add node group" }))
    expect(screen.getByLabelText("Subnet subnet-a")).toBeInTheDocument()
    expect(screen.getByRole("option", { name: "node" })).toBeInTheDocument()
  })

  it("opens the scale dialog seeded with current sizes", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "Scale node group" }))
    expect(screen.getByText("Scale ng-1")).toBeInTheDocument()
    expect(screen.getByLabelText("Min")).toHaveValue(1)
    expect(screen.getByLabelText("Max")).toHaveValue(3)
  })

  it("validates scale sizes before submitting", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "Scale node group" }))
    fireEvent.change(screen.getByLabelText("Min"), { target: { value: "5" } })
    fireEvent.click(screen.getByRole("button", { name: "Scale" }))
    expect(
      screen.getByText("Sizes must satisfy min ≤ desired ≤ max"),
    ).toBeInTheDocument()
  })

  it("surfaces an upgrade affordance when the node group lags the cluster", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.30"
        vpcId="vpc-1"
      />,
      seed({ nodegroupVersion: "1.29" }),
    )
    expect(
      screen.getByRole("button", { name: "Update node group version" }),
    ).toBeInTheDocument()
    expect(screen.getByText("1.29 (cluster on 1.30)")).toBeInTheDocument()
  })

  it("opens the delete confirmation for a node group", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "Delete node group" }))
    expect(
      screen.getByText(
        /Delete node group "ng-1"\? This terminates its nodes\./,
      ),
    ).toBeInTheDocument()
  })

  it("does not badge a standard node group", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    expect(screen.queryByText("GPU")).not.toBeInTheDocument()
  })

  it("badges a GPU node group", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed({ gpuNodegroup: true }),
    )
    expect(screen.getByText("GPU")).toBeInTheDocument()
  })

  it("uses the shared GPU instance-type picker in the add node group form", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "Add node group" }))
    expect(
      screen.getByRole("combobox", { name: "Instance type" }),
    ).toBeInTheDocument()
  })

  it("shows the GPU AMI/taint note only when a GPU instance type is selected", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "Add node group" }))

    expect(
      screen.queryByText(/GPU node AMI is auto-selected/),
    ).not.toBeInTheDocument()

    await user.click(screen.getByRole("combobox", { name: "Instance type" }))
    await user.click(screen.getByRole("option", { name: /g5\.xlarge/ }))

    expect(
      screen.getByText(/GPU node AMI is auto-selected/),
    ).toBeInTheDocument()
    expect(
      screen.getByText("nvidia.com/gpu=present:NoSchedule"),
    ).toBeInTheDocument()
  })
})
