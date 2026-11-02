import type {
  AuthorizeSecurityGroupIngressCommandInput,
  Image,
  InstanceTypeInfo,
  LaunchTemplate,
  PlacementGroup,
  RunInstancesCommandInput,
  Subnet,
} from "@aws-sdk/client-ec2"
import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { instanceTypeCapacityQueryOptions } from "@/queries/admin"
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

import { RunInstancesPage } from "./run-instances-page"

function image(
  id: string,
  name: string,
  architecture: "x86_64" | "arm64",
  rootDevice = "/dev/sda1",
  size = 8,
): Image {
  return {
    ImageId: id,
    Name: name,
    Architecture: architecture,
    RootDeviceName: rootDevice,
    BlockDeviceMappings: [
      { DeviceName: rootDevice, Ebs: { VolumeSize: size } },
    ],
  }
}

const X86_IMAGES = [
  image("ami-u24", "ami-ubuntu-24.04-x86_64", "x86_64"),
  image("ami-u26", "ami-ubuntu-26.04-x86_64", "x86_64"),
  image("ami-o98", "oracle-9.8-x86_64", "x86_64", "/dev/vda"),
  image("ami-o101", "oracle-10.1-x86_64", "x86_64", "/dev/vda", 12),
  image("ami-gold", "golden", "x86_64"),
]

const ARM_IMAGE = image("ami-o101a", "oracle-10.1-arm64", "arm64", "/dev/vda")

function x86(name: string): InstanceTypeInfo {
  return {
    InstanceType: name as InstanceTypeInfo["InstanceType"],
    ProcessorInfo: { SupportedArchitectures: ["x86_64"] },
    VCpuInfo: { DefaultVCpus: 2 },
    MemoryInfo: { SizeInMiB: 512 },
  }
}

const INSTANCE_TYPES: InstanceTypeInfo[] = [
  x86("t3.nano"),
  {
    ...x86("mig.1g.24gb"),
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
]

const CAPACITY = { "mig.1g.24gb": 1, "t3.nano": 2 }

const ARM_TYPE: InstanceTypeInfo = {
  InstanceType: "t4g.nano",
  ProcessorInfo: { SupportedArchitectures: ["arm64"] },
}

const DEFAULT_SUBNET: Subnet = {
  SubnetId: "subnet-def",
  VpcId: "vpc-1",
  CidrBlock: "172.31.0.0/20",
  AvailabilityZone: "ap-southeast-2a",
  DefaultForAz: true,
  MapPublicIpOnLaunch: true,
}

interface SetupOptions {
  images?: Image[]
  instanceTypes?: InstanceTypeInfo[]
  capacity?: Record<string, number>
  subnets?: Subnet[]
  launchTemplates?: LaunchTemplate[]
  placementGroups?: PlacementGroup[]
  onClose?: (instanceId?: string) => void
}

function setup({
  images = X86_IMAGES,
  instanceTypes = INSTANCE_TYPES,
  capacity = CAPACITY,
  subnets = [DEFAULT_SUBNET],
  launchTemplates = [],
  placementGroups = [],
  onClose = vi.fn(),
}: SetupOptions = {}) {
  sdk.reset()
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(ec2ImagesQueryOptions.queryKey, {
    $metadata: {},
    Images: images,
  })
  queryClient.setQueryData(ec2KeyPairsQueryOptions.queryKey, {
    $metadata: {},
    KeyPairs: [{ KeyPairId: "kp-1", KeyName: "default" }],
  })
  queryClient.setQueryData(ec2InstanceTypesQueryOptions.queryKey, {
    $metadata: {},
    InstanceTypes: instanceTypes,
  })
  queryClient.setQueryData(instanceTypeCapacityQueryOptions.queryKey, {
    available: capacity,
    nodes: 1,
  })
  queryClient.setQueryData(ec2SubnetsQueryOptions.queryKey, {
    $metadata: {},
    Subnets: subnets,
  })
  queryClient.setQueryData(ec2PlacementGroupsQueryOptions.queryKey, {
    $metadata: {},
    PlacementGroups: placementGroups,
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
    LaunchTemplates: launchTemplates,
  })

  return renderWithClient(<RunInstancesPage onClose={onClose} />, queryClient)
}

function osTile(name: string) {
  return within(
    screen.getByRole("group", { name: "Operating system" }),
  ).getByRole("radio", { name })
}

describe("run-instances OS picker", () => {
  it("starts on the newest Ubuntu and leaves system images out", async () => {
    setup({
      images: [
        {
          ImageId: "ami-rds",
          Name: "spinifex-rds-postgres",
          Architecture: "x86_64",
          ImageOwnerAlias: "system",
          Tags: [{ Key: "spinifex:managed-by", Value: "rds" }],
        },
        ...X86_IMAGES,
      ],
    })

    await expect(screen.findByLabelText("Version")).resolves.toHaveTextContent(
      "Ubuntu 26.04",
    )
    expect(osTile("Ubuntu")).toBeChecked()
    expect(screen.queryByText(/spinifex-rds-postgres/u)).not.toBeInTheDocument()
  })

  it("selects a distro's highest version when its tile is clicked, and lists the rest", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByRole("radio", { name: "Oracle Linux" }))

    expect(screen.getByLabelText("Version")).toHaveTextContent(
      "Oracle Linux 10.1",
    )

    await user.click(screen.getByLabelText("Version"))

    expect(
      screen.getByRole("option", { name: "Oracle Linux 10.1 (latest)" }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("option", { name: "Oracle Linux 9.8" }),
    ).toBeInTheDocument()
  })

  it("keeps images whose names do not parse behind Other images", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByRole("radio", { name: "Other images" }))

    expect(screen.getByLabelText("Image")).toHaveTextContent("golden")
    expect(screen.queryByLabelText("Version")).not.toBeInTheDocument()
  })
})

describe("run-instances architecture", () => {
  it("is not offered when every image is one architecture", async () => {
    setup()

    await screen.findByLabelText("Version")

    expect(
      screen.queryByRole("group", { name: "Architecture" }),
    ).not.toBeInTheDocument()
  })

  it("narrows images and instance types together when switched to arm64", async () => {
    const user = userEvent.setup()
    setup({
      images: [...X86_IMAGES, ARM_IMAGE],
      instanceTypes: [...INSTANCE_TYPES, ARM_TYPE],
    })

    await user.click(await screen.findByRole("radio", { name: "Oracle Linux" }))
    await user.click(screen.getByRole("radio", { name: "arm64" }))

    expect(screen.getByText("ami-o101a")).toBeInTheDocument()
    expect(osTile("Oracle Linux")).toBeChecked()
    expect(
      screen.queryByRole("radio", { name: "Ubuntu" }),
    ).not.toBeInTheDocument()
    expect(screen.getByLabelText("Instance Type")).toHaveTextContent("t4g.nano")

    await user.click(screen.getByLabelText("Instance Type"))

    expect(
      screen.getByRole("option", { name: /t4g\.nano/u }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole("option", { name: /t3\.nano/u }),
    ).not.toBeInTheDocument()
  })
})

describe("run-instances instance type picker", () => {
  it("uses the shared GpuInstanceTypeSelect, grouping standard and GPU types", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByLabelText("Instance Type"))

    expect(screen.getByText("Standard")).toBeInTheDocument()
    expect(screen.getByText("GPU-Accelerated")).toBeInTheDocument()
    expect(
      screen.getByRole("option", { name: /t3\.nano \(2 available\)/u }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("option", {
        name: /mig\.1g\.24gb — 24 GiB GPU \(1 available\)/u,
      }),
    ).toBeInTheDocument()
  })

  it("shows the GPU info line with model, VRAM and MIG profile when a GPU type is selected", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByLabelText("Instance Type"))
    await user.click(screen.getByRole("option", { name: /mig\.1g\.24gb/u }))

    expect(
      screen.getByText("GPU: MIG 1g.24gb · 24 GiB VRAM · MIG 1g.24gb slice"),
    ).toBeInTheDocument()
  })

  it("shows no GPU info line for a standard type", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByLabelText("Instance Type"))
    await user.click(screen.getByRole("option", { name: /^t3\.nano/u }))

    expect(screen.queryByText(/^GPU:/u)).not.toBeInTheDocument()
  })

  it("states the selected type's vCPUs and memory beneath it", async () => {
    const user = userEvent.setup()
    setup()

    await user.click(await screen.findByLabelText("Instance Type"))
    await user.click(screen.getByRole("option", { name: /^t3\.nano/u }))

    expect(screen.getByTestId("instance-type-size")).toHaveTextContent(
      "2 vCPU · 0.5 GiB memory",
    )
  })
})

describe("run-instances count", () => {
  it("reports the free capacity for the selected type", async () => {
    setup()

    await expect(
      screen.findByText("Available capacity for t3.nano: 2"),
    ).resolves.toBeInTheDocument()
  })

  it("refuses a count above the free capacity", async () => {
    const user = userEvent.setup()
    const runInstances = vi.fn(() => ({ Instances: [{ InstanceId: "i-1" }] }))

    setup()
    sdk.setHandler("CreateSecurityGroupCommand", () => ({ GroupId: "sg-new" }))
    sdk.setHandler("AuthorizeSecurityGroupIngressCommand", () => ({}))
    sdk.setHandler("RunInstancesCommand", runInstances)

    const count = await screen.findByLabelText("Number of Instances")
    await user.clear(count)
    await user.type(count, "3")
    await user.click(screen.getByRole("button", { name: "Create Instance" }))

    await expect(
      screen.findByText("Cannot exceed available capacity"),
    ).resolves.toBeInTheDocument()
    expect(runInstances).not.toHaveBeenCalled()
  })

  // A session whose policy does not grant the capacity action reads no figure.
  // Capping at 1 there would refuse a launch the cluster would have taken, so
  // the launch decides and RunInstances answers if it cannot.
  it("neither reports nor caps when the capacity figure is unknown", async () => {
    const user = userEvent.setup()

    const runInstances = vi.fn((_input: RunInstancesCommandInput) => ({
      Instances: [{ InstanceId: "i-1" }],
    }))

    setup({ capacity: {} })
    sdk.setHandler("CreateSecurityGroupCommand", () => ({ GroupId: "sg-new" }))
    sdk.setHandler("AuthorizeSecurityGroupIngressCommand", () => ({}))
    sdk.setHandler("RunInstancesCommand", runInstances)

    const count = await screen.findByLabelText("Number of Instances")
    expect(
      screen.queryByText(/Available capacity for/u),
    ).not.toBeInTheDocument()

    await user.clear(count)
    await user.type(count, "3")
    await user.click(screen.getByRole("button", { name: "Create Instance" }))

    await waitFor(() => {
      expect(runInstances).toHaveBeenCalled()
    })
    expect(runInstances.mock.calls[0]?.[0].MaxCount).toBe(3)
    expect(
      screen.queryByText("Cannot exceed available capacity"),
    ).not.toBeInTheDocument()
  })
})

describe("run-instances root volume", () => {
  it("is open by default and re-prefills the device and minimum from the picked image", async () => {
    const user = userEvent.setup()
    setup()

    await expect(screen.findByLabelText("Device Name")).resolves.toHaveValue(
      "/dev/sda1",
    )
    expect(
      screen.getByText("Minimum 8 GiB, the image's snapshot size."),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("switch", { name: "Delete on termination" }),
    ).toBeChecked()

    await user.click(screen.getByRole("radio", { name: "Oracle Linux" }))

    expect(screen.getByLabelText("Device Name")).toHaveValue("/dev/vda")
    expect(
      screen.getByText("Minimum 12 GiB, the image's snapshot size."),
    ).toBeInTheDocument()
  })
})

describe("run-instances launch template", () => {
  it("is hidden when the account has no launch templates", async () => {
    setup()

    await screen.findByLabelText("Version")

    expect(screen.queryByLabelText("Launch Template")).not.toBeInTheDocument()
  })

  it("is offered when one exists", async () => {
    setup({
      launchTemplates: [
        { LaunchTemplateId: "lt-1", LaunchTemplateName: "web" },
      ],
    })

    await expect(
      screen.findByLabelText("Launch Template"),
    ).resolves.toBeInTheDocument()
  })
})

describe("run-instances placement group", () => {
  it("is hidden when no group can take a launch", async () => {
    setup({
      placementGroups: [
        {
          GroupId: "pg-1",
          GroupName: "gone",
          Strategy: "spread",
          State: "deleted",
        },
      ],
    })

    await screen.findByLabelText("Version")

    expect(screen.queryByLabelText("Placement Group")).not.toBeInTheDocument()
  })

  it("offers only the available groups", async () => {
    const user = userEvent.setup()
    setup({
      placementGroups: [
        {
          GroupId: "pg-1",
          GroupName: "web",
          Strategy: "spread",
          State: "available",
        },
        {
          GroupId: "pg-2",
          GroupName: "old",
          Strategy: "cluster",
          State: "deleting",
        },
      ],
    })

    await user.click(await screen.findByLabelText("Placement Group"))

    await expect(
      screen.findByRole("option", { name: "web (spread)" }),
    ).resolves.toBeInTheDocument()
    expect(
      screen.queryByRole("option", { name: "old (cluster)" }),
    ).not.toBeInTheDocument()
  })
})

describe("run-instances network", () => {
  it("shows the region with its flag and the default subnet's zone", async () => {
    setup()

    const location = await screen.findByRole("radio", {
      name: "ap-southeast-2",
    })

    expect(location).toBeChecked()
    expect(location).toBeDisabled()
    expect(screen.getByText("Sydney · ap-southeast-2a")).toBeInTheDocument()
  })

  it("labels the no-subnet default as the default subnet, with what it means", async () => {
    setup()

    await expect(screen.findByLabelText("Subnet")).resolves.toHaveTextContent(
      "Default: subnet-def",
    )
    expect(
      screen.getByText(
        "172.31.0.0/20 · ap-southeast-2a · vpc-1 · public IP assigned on launch",
      ),
    ).toBeInTheDocument()
  })

  it("warns when the account has no default subnet", async () => {
    setup({ subnets: [] })

    await expect(screen.findByLabelText("Subnet")).resolves.toHaveTextContent(
      "No subnet",
    )
    expect(
      screen.getByText(
        /no default subnet, so an instance launched without one/u,
      ),
    ).toBeInTheDocument()
  })
})

describe("run-instances security group mode", () => {
  it("defaults to creating a group with ping and SSH allowed and switches to existing groups", async () => {
    const user = userEvent.setup()
    setup()

    await expect(
      screen.findByRole("radio", { name: "Create new security group" }),
    ).resolves.toBeChecked()
    expect(screen.getByLabelText("Allow ping")).toBeChecked()
    expect(screen.getByLabelText("Allow SSH")).toBeChecked()
    expect(screen.getByLabelText("Allow HTTP")).not.toBeChecked()

    const rules = screen
      .getAllByRole("switch")
      .map((el) => el.getAttribute("aria-label"))

    expect(rules.indexOf("Allow ping")).toBeLessThan(rules.indexOf("Allow SSH"))

    await user.click(
      screen.getByRole("radio", { name: "Select existing security group" }),
    )

    expect(
      screen.getByText("No security groups in the resolved VPC."),
    ).toBeInTheDocument()
    expect(screen.queryByLabelText("Allow SSH")).not.toBeInTheDocument()
  })
})

describe("run-instances submit", () => {
  it("launches the picked image with no subnet and no block device override", async () => {
    const user = userEvent.setup()

    const runInstances = vi.fn((_input: RunInstancesCommandInput) => ({
      Instances: [{ InstanceId: "i-1" }],
    }))

    setup()
    sdk.setHandler("CreateSecurityGroupCommand", () => ({ GroupId: "sg-new" }))
    sdk.setHandler("AuthorizeSecurityGroupIngressCommand", () => ({}))
    sdk.setHandler("RunInstancesCommand", runInstances)

    await user.click(await screen.findByRole("radio", { name: "Oracle Linux" }))
    await user.click(screen.getByRole("button", { name: "Create Instance" }))

    await waitFor(() => {
      expect(runInstances).toHaveBeenCalled()
    })

    const input = runInstances.mock.calls[0]?.[0]

    expect(input).toMatchObject({
      ImageId: "ami-o101",
      InstanceType: "t3.nano",
      KeyName: "default",
      SecurityGroupIds: ["sg-new"],
    })
    expect(input?.SubnetId).toBeUndefined()
    expect(input?.BlockDeviceMappings).toBeUndefined()
  })

  it("sends no tags when the name is left blank", async () => {
    const user = userEvent.setup()

    const runInstances = vi.fn((_input: RunInstancesCommandInput) => ({
      Instances: [{ InstanceId: "i-1" }],
    }))

    setup()
    sdk.setHandler("CreateSecurityGroupCommand", () => ({ GroupId: "sg-new" }))
    sdk.setHandler("AuthorizeSecurityGroupIngressCommand", () => ({}))
    sdk.setHandler("RunInstancesCommand", runInstances)

    await user.click(await screen.findByRole("radio", { name: "Oracle Linux" }))
    await user.click(screen.getByRole("button", { name: "Create Instance" }))

    await waitFor(() => {
      expect(runInstances).toHaveBeenCalled()
    })
    expect(runInstances.mock.calls[0]?.[0].TagSpecifications).toBeUndefined()
  })

  it("tags the instance with its name, trimmed", async () => {
    const user = userEvent.setup()

    const runInstances = vi.fn((_input: RunInstancesCommandInput) => ({
      Instances: [{ InstanceId: "i-1" }],
    }))

    setup()
    sdk.setHandler("CreateSecurityGroupCommand", () => ({ GroupId: "sg-new" }))
    sdk.setHandler("AuthorizeSecurityGroupIngressCommand", () => ({}))
    sdk.setHandler("RunInstancesCommand", runInstances)

    await user.click(await screen.findByRole("radio", { name: "Oracle Linux" }))
    await user.type(screen.getByLabelText("Name (optional)"), "  web-1 ")

    await user.click(screen.getByRole("button", { name: "Create Instance" }))

    await waitFor(() => {
      expect(runInstances).toHaveBeenCalled()
    })
    expect(runInstances.mock.calls[0]?.[0].TagSpecifications).toStrictEqual([
      { ResourceType: "instance", Tags: [{ Key: "Name", Value: "web-1" }] },
    ])
  })

  it("opens ping and SSH on the new group by default, ping as echo request only", async () => {
    const user = userEvent.setup()

    const authorize = vi.fn(
      (_input: AuthorizeSecurityGroupIngressCommandInput) => ({}),
    )

    setup()
    sdk.setHandler("CreateSecurityGroupCommand", () => ({ GroupId: "sg-new" }))
    sdk.setHandler("AuthorizeSecurityGroupIngressCommand", authorize)
    sdk.setHandler("RunInstancesCommand", () => ({
      Instances: [{ InstanceId: "i-1" }],
    }))

    await user.click(await screen.findByRole("radio", { name: "Oracle Linux" }))
    await user.click(screen.getByRole("button", { name: "Create Instance" }))

    await waitFor(() => {
      expect(authorize).toHaveBeenCalled()
    })
    expect(authorize.mock.calls[0]?.[0].IpPermissions).toStrictEqual([
      {
        IpProtocol: "icmp",
        FromPort: 8,
        ToPort: -1,
        IpRanges: [{ CidrIp: "0.0.0.0/0" }],
      },
      {
        IpProtocol: "tcp",
        FromPort: 22,
        ToPort: 22,
        IpRanges: [{ CidrIp: "0.0.0.0/0" }],
      },
    ])
  })

  it("shows a refused launch beside the button and stays open", async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()

    setup({ onClose })
    sdk.setHandler("CreateSecurityGroupCommand", () => ({ GroupId: "sg-new" }))
    sdk.setHandler("AuthorizeSecurityGroupIngressCommand", () => ({}))
    sdk.setHandler("RunInstancesCommand", () => {
      throw Object.assign(new Error("Not enough capacity for t3.nano."), {
        name: "InsufficientInstanceCapacity",
      })
    })

    await user.click(await screen.findByRole("radio", { name: "Oracle Linux" }))
    await user.click(screen.getByRole("button", { name: "Create Instance" }))

    await expect(
      screen.findByText(
        "InsufficientInstanceCapacity: Not enough capacity for t3.nano.",
      ),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(
      screen.getByRole("button", { name: "Create Instance" }),
    ).toBeEnabled()
  })
})
