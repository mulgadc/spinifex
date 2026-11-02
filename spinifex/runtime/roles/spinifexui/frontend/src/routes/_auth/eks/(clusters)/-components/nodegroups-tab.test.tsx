import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const send = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

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
    expect(screen.getByText("active")).toBeInTheDocument()
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

  it("opens the add node group form in a panel", async () => {
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
    const panel = screen.getByRole("region", { name: "Add node group" })
    expect(within(panel).getByLabelText("Subnet subnet-a")).toBeInTheDocument()
    await user.click(within(panel).getByLabelText("Node IAM role"))
    expect(screen.getByRole("option", { name: "node" })).toBeInTheDocument()
  })

  it("opens the scale form seeded with current sizes", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "Scale node group" }))
    const panel = screen.getByRole("region", { name: "Scale ng-1" })
    expect(within(panel).getByLabelText("Min")).toHaveValue(1)
    expect(within(panel).getByLabelText("Max")).toHaveValue(3)
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
    const panel = screen.getByRole("region", { name: "Scale ng-1" })
    fireEvent.change(within(panel).getByLabelText("Min"), {
      target: { value: "5" },
    })
    fireEvent.click(within(panel).getByRole("button", { name: "Scale" }))
    expect(
      within(panel).getByText("Sizes must satisfy min ≤ desired ≤ max"),
    ).toBeInTheDocument()
    expect(send).not.toHaveBeenCalled()
  })

  it("scales the node group and closes the panel", async () => {
    send.mockClear()
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "Scale node group" }))
    const panel = screen.getByRole("region", { name: "Scale ng-1" })
    fireEvent.change(within(panel).getByLabelText("Max"), {
      target: { value: "5" },
    })
    fireEvent.click(within(panel).getByRole("button", { name: "Scale" }))

    await waitFor(() => {
      expect(
        screen.queryByRole("region", { name: "Scale ng-1" }),
      ).not.toBeInTheDocument()
    })
    expect(send.mock.calls[0]?.[0].input).toMatchObject({
      clusterName: CLUSTER,
      nodegroupName: "ng-1",
      scalingConfig: { minSize: 1, desiredSize: 2, maxSize: 5 },
    })
  })

  it("shows the version lag without an upgrade action when the node group lags the cluster", () => {
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.30"
        vpcId="vpc-1"
      />,
      seed({ nodegroupVersion: "1.29" }),
    )
    expect(screen.getByText("1.29 (cluster on 1.30)")).toBeInTheDocument()
    expect(
      screen.queryByRole("button", { name: "Update node group version" }),
    ).not.toBeInTheDocument()
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

  it("shows a refused create in the panel and keeps it open", async () => {
    send.mockRejectedValueOnce(
      Object.assign(new Error("Nodegroup already exists with name ng-2"), {
        name: "ResourceInUseException",
      }),
    )
    renderWithClient(
      <NodegroupsTab
        clusterName={CLUSTER}
        clusterVersion="1.29"
        vpcId="vpc-1"
      />,
      seed(),
    )
    const user = userEvent.setup()
    fireEvent.click(screen.getByRole("button", { name: "Add node group" }))
    const dialog = screen.getByRole("region", { name: "Add node group" })
    fireEvent.change(within(dialog).getByLabelText("Name"), {
      target: { value: "ng-2" },
    })
    await user.click(within(dialog).getByLabelText("Node IAM role"))
    await user.click(screen.getByRole("option", { name: "node" }))
    await user.click(within(dialog).getByLabelText("Subnet subnet-a"))
    await user.click(within(dialog).getByRole("button", { name: "Create" }))

    await expect(
      within(dialog).findByText(
        "ResourceInUseException: Nodegroup already exists with name ng-2",
      ),
    ).resolves.toBeInTheDocument()
    expect(send.mock.calls[0]?.[0].input).toMatchObject({
      nodegroupName: "ng-2",
      nodeRole: "arn:aws:iam::0:role/node",
      subnets: ["subnet-a"],
    })
    expect(screen.getByLabelText("Name")).toHaveValue("ng-2")
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeEnabled()
  })
})
