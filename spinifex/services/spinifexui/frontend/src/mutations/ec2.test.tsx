import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getEc2Client: () => ({ send: mockSend }),
}))

import type { CreateVpcWizardFormData } from "@/types/ec2"

import {
  useAttachVolume,
  useAuthorizeSecurityGroupEgress,
  useAuthorizeSecurityGroupIngress,
  useCopySnapshot,
  useCreateImage,
  useCreateInstance,
  useCreateKeyPair,
  useCreatePlacementGroup,
  useCreateSecurityGroup,
  useCreateSnapshot,
  useCreateSubnet,
  useCreateVolume,
  useCreateVpc,
  useCreateVpcWizard,
  useDeleteKeyPair,
  useDeletePlacementGroup,
  useDeleteSecurityGroup,
  useDeleteSnapshot,
  useDeleteSubnet,
  useDeleteVolume,
  useDeleteVpc,
  useDetachVolume,
  useGetConsoleOutput,
  useImportKeyPair,
  useModifyInstanceAttribute,
  useModifyVolume,
  useRebootInstance,
  useRevokeSecurityGroupEgress,
  useRevokeSecurityGroupIngress,
  useStartInstance,
  useStopInstance,
  useTerminateInstance,
} from "./ec2"

let queryClient: QueryClient

function wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function createQueryClient() {
  queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  return queryClient
}

describe("useStartInstance", () => {
  it("sends StartInstancesCommand with the instance ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useStartInstance(), { wrapper })

    result.current.mutate("i-abc123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend).toHaveBeenCalledOnce()

    const command = mockSend.mock.calls[0]?.[0]
    expect(command.input).toStrictEqual({ InstanceIds: ["i-abc123"] })
  })

  it("invalidates instances query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useStartInstance(), { wrapper })

    result.current.mutate("i-abc123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["ec2", "instances"] })
  })
})

describe("useStopInstance", () => {
  it("sends StopInstancesCommand with the instance ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useStopInstance(), { wrapper })

    result.current.mutate("i-abc123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceIds: ["i-abc123"],
    })
  })
})

describe("useTerminateInstance", () => {
  it("sends TerminateInstancesCommand with the instance ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useTerminateInstance(), { wrapper })

    result.current.mutate("i-abc123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceIds: ["i-abc123"],
    })
  })
})

describe("useCreateInstance", () => {
  it("sends RunInstancesCommand with form data", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateInstance(), { wrapper })

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 2,
      subnetId: "subnet-1",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toMatchObject({
      ImageId: "ami-123",
      InstanceType: "t2.micro",
      KeyName: "my-key",
      MinCount: 2,
      MaxCount: 2,
      SubnetId: "subnet-1",
    })
  })

  it("omits SubnetId when empty string", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateInstance(), { wrapper })

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      subnetId: "",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.SubnetId).toBeUndefined()
  })

  it("includes Placement when placementGroupName is provided", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateInstance(), { wrapper })

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      placementGroupName: "my-group",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Placement).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("omits Placement when placementGroupName is empty", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateInstance(), { wrapper })

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      placementGroupName: "",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Placement).toBeUndefined()
  })

  it("omits BlockDeviceMappings when no storage fields are set", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateInstance(), { wrapper })

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootDeviceName: "/dev/sda1",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(
      mockSend.mock.calls[0]?.[0].input.BlockDeviceMappings,
    ).toBeUndefined()
  })

  it("includes BlockDeviceMappings when root volume size is set", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateInstance(), { wrapper })

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootDeviceName: "/dev/sda1",
      rootVolumeSize: 100,
      rootVolumeType: "gp3",
      rootDeleteOnTermination: true,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.BlockDeviceMappings).toStrictEqual(
      [
        {
          DeviceName: "/dev/sda1",
          Ebs: {
            VolumeSize: 100,
            VolumeType: "gp3",
            DeleteOnTermination: true,
          },
        },
      ],
    )
  })

  it("includes only the storage fields the user set", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateInstance(), { wrapper })

    result.current.mutate({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootDeviceName: "/dev/sda1",
      rootDeleteOnTermination: false,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.BlockDeviceMappings).toStrictEqual(
      [
        {
          DeviceName: "/dev/sda1",
          Ebs: {
            DeleteOnTermination: false,
          },
        },
      ],
    )
  })
})

describe("useImportKeyPair", () => {
  it("strips the comment from an SSH public key", async () => {
    createQueryClient()
    const { result } = renderHook(() => useImportKeyPair(), { wrapper })

    result.current.mutate({
      keyName: "my-key",
      publicKeyMaterial: "ssh-rsa AAAAB3Nza... user@host",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.KeyName).toBe("my-key")

    const decoded = new TextDecoder().decode(input.PublicKeyMaterial)
    expect(decoded).toBe("ssh-rsa AAAAB3Nza...")
    expect(decoded).not.toContain("user@host")
  })

  it("handles keys without comments", async () => {
    createQueryClient()
    const { result } = renderHook(() => useImportKeyPair(), { wrapper })

    result.current.mutate({
      keyName: "my-key",
      publicKeyMaterial: "ssh-rsa AAAAB3Nza...",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const decoded = new TextDecoder().decode(
      mockSend.mock.calls[0]?.[0].input.PublicKeyMaterial,
    )
    expect(decoded).toBe("ssh-rsa AAAAB3Nza...")
  })

  it("handles extra whitespace in key", async () => {
    createQueryClient()
    const { result } = renderHook(() => useImportKeyPair(), { wrapper })

    result.current.mutate({
      keyName: "my-key",
      publicKeyMaterial: "  ssh-rsa   AAAAB3Nza...   user@host  ",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const decoded = new TextDecoder().decode(
      mockSend.mock.calls[0]?.[0].input.PublicKeyMaterial,
    )
    expect(decoded).toBe("ssh-rsa AAAAB3Nza...")
  })
})

describe("useRebootInstance", () => {
  it("sends RebootInstancesCommand with the instance ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRebootInstance(), { wrapper })

    result.current.mutate("i-abc123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceIds: ["i-abc123"],
    })
  })
})

describe("useCreateKeyPair", () => {
  it("sends CreateKeyPairCommand with key name and rsa type", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateKeyPair(), { wrapper })

    result.current.mutate({ keyName: "my-key" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      KeyName: "my-key",
      KeyType: "rsa",
    })
  })
})

describe("useDeleteKeyPair", () => {
  it("sends DeleteKeyPairCommand with key pair ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteKeyPair(), { wrapper })

    result.current.mutate("kp-abc123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      KeyPairId: "kp-abc123",
    })
  })
})

describe("useCreateVolume", () => {
  it("sends CreateVolumeCommand with size and availability zone", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateVolume(), { wrapper })

    result.current.mutate({ size: 100, availabilityZone: "us-east-1a" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Size: 100,
      AvailabilityZone: "us-east-1a",
      VolumeType: "gp3",
    })
  })
})

describe("useModifyVolume", () => {
  it("sends ModifyVolumeCommand with volume ID and new size", async () => {
    createQueryClient()
    const { result } = renderHook(() => useModifyVolume(), { wrapper })

    result.current.mutate({ volumeId: "vol-123", size: 200 })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
      Size: 200,
    })
  })
})

describe("useDeleteVolume", () => {
  it("sends DeleteVolumeCommand with volume ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteVolume(), { wrapper })

    result.current.mutate("vol-123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
    })
  })
})

describe("useCreateSnapshot", () => {
  it("sends CreateSnapshotCommand with volume ID and description", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateSnapshot(), { wrapper })

    result.current.mutate({ volumeId: "vol-123", description: "backup" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
      Description: "backup",
    })
  })

  it("omits Description when empty", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateSnapshot(), { wrapper })

    result.current.mutate({ volumeId: "vol-123", description: "" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Description).toBeUndefined()
  })
})

describe("useDeleteSnapshot", () => {
  it("sends DeleteSnapshotCommand with snapshot ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteSnapshot(), { wrapper })

    result.current.mutate("snap-123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SnapshotId: "snap-123",
    })
  })
})

describe("useCopySnapshot", () => {
  it("sends CopySnapshotCommand with source details", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCopySnapshot(), { wrapper })

    result.current.mutate({
      sourceSnapshotId: "snap-123",
      sourceRegion: "us-east-1",
      description: "copy",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SourceSnapshotId: "snap-123",
      SourceRegion: "us-east-1",
      Description: "copy",
    })
  })
})

describe("useAttachVolume", () => {
  it("sends AttachVolumeCommand with volume, instance, and device", async () => {
    createQueryClient()
    const { result } = renderHook(() => useAttachVolume(), { wrapper })

    result.current.mutate({
      volumeId: "vol-123",
      instanceId: "i-abc",
      device: "/dev/sdf",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
      InstanceId: "i-abc",
      Device: "/dev/sdf",
    })
  })
})

describe("useDetachVolume", () => {
  it("sends DetachVolumeCommand with volume, instance, and force", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDetachVolume(), { wrapper })

    result.current.mutate({
      volumeId: "vol-123",
      instanceId: "i-abc",
      force: true,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeId: "vol-123",
      InstanceId: "i-abc",
      Force: true,
    })
  })
})

describe("useModifyInstanceAttribute", () => {
  it("sends ModifyInstanceAttributeCommand with instance type", async () => {
    createQueryClient()
    const { result } = renderHook(() => useModifyInstanceAttribute(), {
      wrapper,
    })

    result.current.mutate({
      instanceId: "i-abc",
      instanceType: "t3.large",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceId: "i-abc",
      InstanceType: { Value: "t3.large" },
    })
  })
})

describe("useGetConsoleOutput", () => {
  it("sends GetConsoleOutputCommand with instance ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useGetConsoleOutput(), { wrapper })

    result.current.mutate("i-abc123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceId: "i-abc123",
    })
  })
})

describe("useCreateImage", () => {
  it("sends CreateImageCommand with instance ID and name", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateImage(), { wrapper })

    result.current.mutate({
      instanceId: "i-abc",
      name: "my-image",
      description: "test image",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceId: "i-abc",
      Name: "my-image",
      Description: "test image",
    })
  })
})

describe("useCreateVpc", () => {
  it("includes TagSpecifications when name is provided", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateVpc(), { wrapper })

    result.current.mutate({ cidrBlock: "10.0.0.0/16", name: "my-vpc" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      CidrBlock: "10.0.0.0/16",
      TagSpecifications: [
        {
          ResourceType: "vpc",
          Tags: [{ Key: "Name", Value: "my-vpc" }],
        },
      ],
    })
  })

  it("omits TagSpecifications when name is empty", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateVpc(), { wrapper })

    result.current.mutate({ cidrBlock: "10.0.0.0/16", name: "" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.TagSpecifications).toBeUndefined()
  })
})

describe("useDeleteVpc", () => {
  it("sends DeleteVpcCommand with VPC ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteVpc(), { wrapper })

    result.current.mutate("vpc-123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VpcId: "vpc-123",
    })
  })
})

describe("useCreateSubnet", () => {
  it("sends CreateSubnetCommand with VPC ID and CIDR block", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateSubnet(), { wrapper })

    result.current.mutate({
      vpcId: "vpc-123",
      cidrBlock: "10.0.1.0/24",
      availabilityZone: "us-east-1a",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VpcId: "vpc-123",
      CidrBlock: "10.0.1.0/24",
      AvailabilityZone: "us-east-1a",
    })
  })
})

describe("useDeleteSubnet", () => {
  it("sends DeleteSubnetCommand with subnet ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteSubnet(), { wrapper })

    result.current.mutate("subnet-123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SubnetId: "subnet-123",
    })
  })
})

describe("useCreatePlacementGroup", () => {
  it("sends CreatePlacementGroupCommand with group name and strategy", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreatePlacementGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group", strategy: "spread" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      Strategy: "spread",
    })
  })

  it("invalidates placementGroups query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreatePlacementGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group", strategy: "cluster" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "placementGroups"],
    })
  })
})

describe("useDeletePlacementGroup", () => {
  it("sends DeletePlacementGroupCommand with group name", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeletePlacementGroup(), { wrapper })

    result.current.mutate("my-group")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("invalidates placementGroups query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useDeletePlacementGroup(), { wrapper })

    result.current.mutate("my-group")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "placementGroups"],
    })
  })
})

describe("useCreateSecurityGroup", () => {
  it("sends CreateSecurityGroupCommand with form data", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateSecurityGroup(), { wrapper })

    result.current.mutate({
      groupName: "web-sg",
      description: "Allow web traffic",
      vpcId: "vpc-123",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "web-sg",
      Description: "Allow web traffic",
      VpcId: "vpc-123",
    })
  })

  it("invalidates securityGroups query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateSecurityGroup(), { wrapper })

    result.current.mutate({
      groupName: "web-sg",
      description: "Allow web traffic",
      vpcId: "vpc-123",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups"],
    })
  })
})

describe("useDeleteSecurityGroup", () => {
  it("sends DeleteSecurityGroupCommand with group ID", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteSecurityGroup(), { wrapper })

    result.current.mutate("sg-123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-123",
    })
  })

  it("invalidates securityGroups query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useDeleteSecurityGroup(), { wrapper })

    result.current.mutate("sg-123")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups"],
    })
  })
})

describe("useAuthorizeSecurityGroupIngress", () => {
  const ruleParams = {
    groupId: "sg-123",
    ipProtocol: "tcp",
    fromPort: 443,
    toPort: 443,
    cidrIp: "0.0.0.0/0",
  }

  it("sends AuthorizeSecurityGroupIngressCommand with IpPermissions", async () => {
    createQueryClient()
    const { result } = renderHook(() => useAuthorizeSecurityGroupIngress(), {
      wrapper,
    })

    result.current.mutate(ruleParams)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-123",
      IpPermissions: [
        {
          IpProtocol: "tcp",
          FromPort: 443,
          ToPort: 443,
          IpRanges: [{ CidrIp: "0.0.0.0/0" }],
        },
      ],
    })
  })

  it("invalidates securityGroups queries on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useAuthorizeSecurityGroupIngress(), {
      wrapper,
    })

    result.current.mutate(ruleParams)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups"],
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups", "sg-123"],
    })
  })
})

describe("useAuthorizeSecurityGroupEgress", () => {
  it("sends AuthorizeSecurityGroupEgressCommand with IpPermissions", async () => {
    createQueryClient()
    const { result } = renderHook(() => useAuthorizeSecurityGroupEgress(), {
      wrapper,
    })

    result.current.mutate({
      groupId: "sg-456",
      ipProtocol: "-1",
      fromPort: -1,
      toPort: -1,
      cidrIp: "0.0.0.0/0",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-456",
      IpPermissions: [
        {
          IpProtocol: "-1",
          FromPort: -1,
          ToPort: -1,
          IpRanges: [{ CidrIp: "0.0.0.0/0" }],
        },
      ],
    })
  })
})

describe("useRevokeSecurityGroupIngress", () => {
  it("sends RevokeSecurityGroupIngressCommand with IpPermissions", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRevokeSecurityGroupIngress(), {
      wrapper,
    })

    result.current.mutate({
      groupId: "sg-123",
      ipProtocol: "tcp",
      fromPort: 22,
      toPort: 22,
      cidrIp: "10.0.0.0/16",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-123",
      IpPermissions: [
        {
          IpProtocol: "tcp",
          FromPort: 22,
          ToPort: 22,
          IpRanges: [{ CidrIp: "10.0.0.0/16" }],
        },
      ],
    })
  })

  it("invalidates securityGroups queries on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useRevokeSecurityGroupIngress(), {
      wrapper,
    })

    result.current.mutate({
      groupId: "sg-123",
      ipProtocol: "tcp",
      fromPort: 22,
      toPort: 22,
      cidrIp: "10.0.0.0/16",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups"],
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "securityGroups", "sg-123"],
    })
  })
})

describe("useRevokeSecurityGroupEgress", () => {
  it("sends RevokeSecurityGroupEgressCommand with IpPermissions", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRevokeSecurityGroupEgress(), {
      wrapper,
    })

    result.current.mutate({
      groupId: "sg-456",
      ipProtocol: "udp",
      fromPort: 53,
      toPort: 53,
      cidrIp: "0.0.0.0/0",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupId: "sg-456",
      IpPermissions: [
        {
          IpProtocol: "udp",
          FromPort: 53,
          ToPort: 53,
          IpRanges: [{ CidrIp: "0.0.0.0/0" }],
        },
      ],
    })
  })
})

describe("useCreateVpcWizard", () => {
  const baseParams: CreateVpcWizardFormData = {
    mode: "vpc-only",
    namePrefix: "test",
    autoGenerateNames: true,
    cidrBlock: "10.0.0.0/16",
    tenancy: "default",
    publicSubnetCount: 0,
    privateSubnetCount: 0,
    publicSubnetCidrs: [],
    privateSubnetCidrs: [],
    tags: [],
  }

  it("sends only CreateVpcCommand in vpc-only mode", async () => {
    createQueryClient()
    mockSend.mockResolvedValueOnce({
      Vpc: { VpcId: "vpc-111" },
    })
    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate(baseParams)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input.CidrBlock).toBe("10.0.0.0/16")
    expect(result.current.data?.vpcId).toBe("vpc-111")
    expect(result.current.data?.created).toHaveLength(1)
  })

  it("includes TagSpecifications with auto-generated name", async () => {
    createQueryClient()
    mockSend.mockResolvedValueOnce({
      Vpc: { VpcId: "vpc-111" },
    })
    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate({
      ...baseParams,
      namePrefix: "proj",
      autoGenerateNames: true,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const tags = mockSend.mock.calls[0]?.[0].input.TagSpecifications
    expect(tags?.[0]?.Tags).toContainEqual({
      Key: "Name",
      Value: "proj-vpc",
    })
  })

  it("sends correct command sequence for vpc-and-more with 1 public + 1 private", async () => {
    createQueryClient()
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-pub-1" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-priv-1" } })
      .mockResolvedValueOnce({
        InternetGateway: { InternetGatewayId: "igw-111" },
      })
      // AttachInternetGateway
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({
        RouteTable: { RouteTableId: "rtb-pub-1" },
      })
      // CreateRoute
      .mockResolvedValueOnce({})
      // AssociateRouteTable (public)
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({
        RouteTable: { RouteTableId: "rtb-priv-1" },
      })
      // AssociateRouteTable (private)
      .mockResolvedValueOnce({})

    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate({
      ...baseParams,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend).toHaveBeenCalledTimes(10)
    expect(result.current.data?.vpcId).toBe("vpc-111")
    expect(result.current.data?.created).toHaveLength(6)
    expect(result.current.data?.error).toBeUndefined()
  })

  it("skips IGW and public route table when publicSubnetCount is 0", async () => {
    createQueryClient()
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-priv-1" } })
      .mockResolvedValueOnce({
        RouteTable: { RouteTableId: "rtb-priv-1" },
      })
      // AssociateRouteTable
      .mockResolvedValueOnce({})

    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate({
      ...baseParams,
      mode: "vpc-and-more",
      publicSubnetCount: 0,
      privateSubnetCount: 1,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend).toHaveBeenCalledTimes(4)
    const types = result.current.data?.created.map((r) => r.type)
    expect(types).not.toContain("Internet Gateway")
    expect(types).not.toContain("Public Route Table")
  })

  it("returns partial result with failedStep on error mid-orchestration", async () => {
    createQueryClient()
    mockSend
      .mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
      .mockResolvedValueOnce({ Subnet: { SubnetId: "subnet-pub-1" } })
      .mockRejectedValueOnce(new Error("CIDR conflict"))

    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate({
      ...baseParams,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(result.current.data?.error?.message).toBe("CIDR conflict")
    expect(result.current.data?.failedStep).toBe(
      "Failed while creating private subnets",
    )
    expect(result.current.data?.vpcId).toBe("vpc-111")
    expect(result.current.data?.created).toHaveLength(2)
  })

  it("returns error when VPC creation fails", async () => {
    createQueryClient()
    mockSend.mockRejectedValueOnce(new Error("Access denied"))

    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate(baseParams)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(result.current.data?.error?.message).toBe("Access denied")
    expect(result.current.data?.failedStep).toBe("Failed while creating VPC")
    expect(result.current.data?.vpcId).toBeUndefined()
    expect(result.current.data?.created).toHaveLength(0)
  })

  it("returns error when VPC is created but no ID returned", async () => {
    createQueryClient()
    mockSend.mockResolvedValueOnce({ Vpc: {} })

    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate(baseParams)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(result.current.data?.error?.message).toContain(
      "no VPC ID was returned",
    )
    expect(result.current.data?.failedStep).toBe("Failed while creating VPC")
  })

  it("invalidates related queries on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    mockSend.mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })
    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate(baseParams)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["ec2", "vpcs"] })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["ec2", "subnets"] })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["ec2", "internetGateways"],
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["ec2", "routeTables"] })
  })

  it("propagates extra tags to all resources", async () => {
    createQueryClient()
    mockSend.mockResolvedValueOnce({ Vpc: { VpcId: "vpc-111" } })

    const { result } = renderHook(() => useCreateVpcWizard(), { wrapper })

    result.current.mutate({
      ...baseParams,
      tags: [{ key: "Env", value: "prod" }],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const tags = mockSend.mock.calls[0]?.[0].input.TagSpecifications?.[0]?.Tags
    expect(tags).toContainEqual({ Key: "Env", Value: "prod" })
  })
})
