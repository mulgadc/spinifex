import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => vi.fn(),
    useSearch: () => ({}),
    Link: ({ children, to }: { children: ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2LaunchTemplatesQueryOptions,
  ec2PlacementGroupsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"

import { CreateInstance } from "./run-instances"

const INSTANCE_TYPES = [
  { InstanceType: "t3.nano" },
  { InstanceType: "t3.nano" },
  {
    InstanceType: "mig.1g.24gb",
    GpuInfo: {
      Gpus: [
        {
          Name: "MIG 1g.24gb",
          Manufacturer: "NVIDIA",
          Count: 1,
          MemoryInfo: { SizeInMiB: 24_576 },
        },
      ],
      TotalGpuMemoryInMiB: 24_576,
    },
  },
] as InstanceTypeInfo[]

function setup() {
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(ec2ImagesQueryOptions.queryKey, {
    $metadata: {},
    Images: [
      {
        ImageId: "ami-1",
        Name: "ubuntu-24.04",
        Architecture: "x86_64",
        RootDeviceName: "/dev/sda1",
        BlockDeviceMappings: [
          { DeviceName: "/dev/sda1", Ebs: { VolumeSize: 8 } },
        ],
      },
    ],
  })
  queryClient.setQueryData(ec2KeyPairsQueryOptions.queryKey, {
    $metadata: {},
    KeyPairs: [{ KeyPairId: "kp-1", KeyName: "default" }],
  })
  queryClient.setQueryData(ec2InstanceTypesQueryOptions.queryKey, {
    $metadata: {},
    InstanceTypes: INSTANCE_TYPES,
  })
  queryClient.setQueryData(ec2SubnetsQueryOptions.queryKey, {
    $metadata: {},
    Subnets: [],
  })
  queryClient.setQueryData(ec2PlacementGroupsQueryOptions.queryKey, {
    $metadata: {},
    PlacementGroups: [],
  })
  queryClient.setQueryData(ec2VpcsQueryOptions.queryKey, {
    $metadata: {},
    Vpcs: [{ VpcId: "vpc-1", IsDefault: true }],
  })
  queryClient.setQueryData(ec2SecurityGroupsQueryOptions.queryKey, {
    $metadata: {},
    SecurityGroups: [],
  })
  queryClient.setQueryData(ec2LaunchTemplatesQueryOptions.queryKey, {
    $metadata: {},
    LaunchTemplates: [],
  })
  return renderWithClient(<CreateInstance />, queryClient)
}

describe("run-instances instance type picker", () => {
  it("uses the shared GpuInstanceTypeSelect, grouping standard and GPU types", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByLabelText("Instance Type"))

    expect(screen.getByText("Standard")).toBeInTheDocument()
    expect(screen.getByText("GPU-Accelerated")).toBeInTheDocument()
    expect(
      screen.getByRole("option", { name: /t3\.nano \(2 available\)/ }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("option", {
        name: /mig\.1g\.24gb — 24 GiB GPU \(1 available\)/,
      }),
    ).toBeInTheDocument()
  })

  it("shows the GPU info line with model, VRAM and MIG profile when a GPU type is selected", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByLabelText("Instance Type"))
    await user.click(screen.getByRole("option", { name: /mig\.1g\.24gb/ }))

    expect(
      screen.getByText("GPU: MIG 1g.24gb · 24 GiB VRAM · MIG 1g.24gb slice"),
    ).toBeInTheDocument()
  })

  it("shows no GPU info line for a standard type", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByLabelText("Instance Type"))
    await user.click(screen.getByRole("option", { name: /^t3\.nano/ }))

    expect(screen.queryByText(/^GPU:/)).not.toBeInTheDocument()
  })
})
