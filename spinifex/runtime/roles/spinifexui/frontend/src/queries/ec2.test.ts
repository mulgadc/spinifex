import { afterEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getEc2Client: () => ({ send: mockSend }),
}))

import { callQueryFn } from "@/test/query"

import {
  ec2AvailabilityZonesQueryOptions,
  ec2IamInstanceProfileAssociationsQueryOptions,
  ec2ImageQueryOptions,
  ec2ImagesQueryOptions,
  ec2InstanceQueryOptions,
  ec2InstancesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2LaunchTemplateVersionsQueryOptions,
  ec2LaunchTemplatesQueryOptions,
  ec2PlacementGroupQueryOptions,
  ec2PlacementGroupsQueryOptions,
  ec2RegionsQueryOptions,
  ec2SecurityGroupQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SnapshotQueryOptions,
  ec2SnapshotsQueryOptions,
  ec2SubnetQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VolumeQueryOptions,
  ec2VolumesQueryOptions,
  ec2VpcQueryOptions,
  ec2VpcsQueryOptions,
} from "./ec2"

describe("query keys", () => {
  it("ec2InstancesQueryOptions has correct key", () => {
    expect(ec2InstancesQueryOptions.queryKey).toStrictEqual([
      "ec2",
      "instances",
    ])
  })

  it("ec2InstanceQueryOptions includes instanceId in key", () => {
    expect(ec2InstanceQueryOptions("i-123").queryKey).toStrictEqual([
      "ec2",
      "instances",
      "i-123",
    ])
  })

  it("ec2IamInstanceProfileAssociationsQueryOptions includes instanceId in key", () => {
    expect(
      ec2IamInstanceProfileAssociationsQueryOptions("i-123").queryKey,
    ).toStrictEqual(["ec2", "iam-instance-profile-associations", "i-123"])
  })

  it("ec2ImagesQueryOptions has correct key", () => {
    expect(ec2ImagesQueryOptions.queryKey).toStrictEqual(["ec2", "images"])
  })

  it("ec2ImageQueryOptions uses 'none' for undefined imageId", () => {
    expect(ec2ImageQueryOptions(undefined).queryKey).toStrictEqual([
      "ec2",
      "images",
      "none",
    ])
  })

  it("ec2KeyPairsQueryOptions has correct key", () => {
    expect(ec2KeyPairsQueryOptions.queryKey).toStrictEqual(["ec2", "keypairs"])
  })

  it("ec2KeyPairQueryOptions includes keyPairId", () => {
    expect(ec2KeyPairQueryOptions("kp-abc").queryKey).toStrictEqual([
      "ec2",
      "keypairs",
      "kp-abc",
    ])
  })

  it("ec2VolumesQueryOptions has correct key", () => {
    expect(ec2VolumesQueryOptions.queryKey).toStrictEqual(["ec2", "volumes"])
  })

  it("ec2VolumeQueryOptions includes volumeId", () => {
    expect(ec2VolumeQueryOptions("vol-1").queryKey).toStrictEqual([
      "ec2",
      "volumes",
      "vol-1",
    ])
  })

  it("ec2SnapshotsQueryOptions has correct key", () => {
    expect(ec2SnapshotsQueryOptions.queryKey).toStrictEqual([
      "ec2",
      "snapshots",
    ])
  })

  it("ec2SnapshotQueryOptions includes snapshotId", () => {
    expect(ec2SnapshotQueryOptions("snap-1").queryKey).toStrictEqual([
      "ec2",
      "snapshots",
      "snap-1",
    ])
  })

  it("ec2VpcsQueryOptions has correct key", () => {
    expect(ec2VpcsQueryOptions.queryKey).toStrictEqual(["ec2", "vpcs"])
  })

  it("ec2VpcQueryOptions includes vpcId", () => {
    expect(ec2VpcQueryOptions("vpc-1").queryKey).toStrictEqual([
      "ec2",
      "vpcs",
      "vpc-1",
    ])
  })

  it("ec2SubnetsQueryOptions has correct key", () => {
    expect(ec2SubnetsQueryOptions.queryKey).toStrictEqual(["ec2", "subnets"])
  })

  it("ec2SubnetQueryOptions includes subnetId", () => {
    expect(ec2SubnetQueryOptions("subnet-1").queryKey).toStrictEqual([
      "ec2",
      "subnets",
      "subnet-1",
    ])
  })

  it("ec2InstanceTypesQueryOptions has correct key", () => {
    expect(ec2InstanceTypesQueryOptions.queryKey).toStrictEqual([
      "ec2",
      "instances",
      "types",
    ])
  })

  it("ec2PlacementGroupsQueryOptions has correct key", () => {
    expect(ec2PlacementGroupsQueryOptions.queryKey).toStrictEqual([
      "ec2",
      "placementGroups",
    ])
  })

  it("ec2PlacementGroupQueryOptions includes groupId in key", () => {
    expect(ec2PlacementGroupQueryOptions("pg-123").queryKey).toStrictEqual([
      "ec2",
      "placementGroups",
      "pg-123",
    ])
  })

  it("ec2SecurityGroupsQueryOptions has correct key", () => {
    expect(ec2SecurityGroupsQueryOptions.queryKey).toStrictEqual([
      "ec2",
      "securityGroups",
    ])
  })

  it("ec2SecurityGroupQueryOptions includes groupId in key", () => {
    expect(ec2SecurityGroupQueryOptions("sg-123").queryKey).toStrictEqual([
      "ec2",
      "securityGroups",
      "sg-123",
    ])
  })

  it("ec2LaunchTemplatesQueryOptions has correct key", () => {
    expect(ec2LaunchTemplatesQueryOptions.queryKey).toStrictEqual([
      "ec2",
      "launchTemplates",
    ])
  })

  it("ec2LaunchTemplateVersionsQueryOptions includes id and versions in key", () => {
    expect(
      ec2LaunchTemplateVersionsQueryOptions("lt-123").queryKey,
    ).toStrictEqual(["ec2", "launchTemplates", "lt-123", "versions"])
  })
})

describe("staleTime and refetchInterval", () => {
  it("availability zones use staleTime", () => {
    expect(ec2AvailabilityZonesQueryOptions.staleTime).toBe(300_000)
  })

  it("regions use staleTime", () => {
    expect(ec2RegionsQueryOptions.staleTime).toBe(300_000)
  })

  it("instances refetch on interval", () => {
    expect(ec2InstancesQueryOptions.refetchInterval).toBe(5000)
  })

  it("volumes refetch on interval", () => {
    expect(ec2VolumesQueryOptions.refetchInterval).toBe(5000)
  })

  it("snapshots refetch on interval", () => {
    expect(ec2SnapshotsQueryOptions.refetchInterval).toBe(5000)
  })

  it("instance types refetch on interval", () => {
    expect(ec2InstanceTypesQueryOptions.refetchInterval).toBe(5000)
  })

  it("launch templates refetch on interval", () => {
    expect(ec2LaunchTemplatesQueryOptions.refetchInterval).toBe(5000)
  })

  it("launch template versions refetch on interval", () => {
    expect(ec2LaunchTemplateVersionsQueryOptions("lt-1").refetchInterval).toBe(
      5000,
    )
  })
})

describe("queryFn", () => {
  afterEach(() => {
    mockSend.mockClear()
  })

  it("ec2ImageQueryOptions returns empty result for undefined imageId", async () => {
    const options = ec2ImageQueryOptions(undefined)
    const result = await callQueryFn(options)
    expect(result).toStrictEqual({ Images: [], $metadata: {} })
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("ec2ImageQueryOptions sends DescribeImagesCommand for valid imageId", async () => {
    await callQueryFn(ec2ImageQueryOptions("ami-123"))
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      ImageIds: ["ami-123"],
    })
  })

  it("ec2InstancesQueryOptions sends DescribeInstancesCommand", async () => {
    await callQueryFn(ec2InstancesQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("ec2LaunchTemplatesQueryOptions sends DescribeLaunchTemplatesCommand", async () => {
    await callQueryFn(ec2LaunchTemplatesQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("ec2LaunchTemplateVersionsQueryOptions sends command with ID", async () => {
    await callQueryFn(ec2LaunchTemplateVersionsQueryOptions("lt-123"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      LaunchTemplateId: "lt-123",
    })
  })

  it("ec2InstanceQueryOptions sends DescribeInstancesCommand with ID", async () => {
    await callQueryFn(ec2InstanceQueryOptions("i-123"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceIds: ["i-123"],
    })
  })

  it("ec2KeyPairsQueryOptions sends DescribeKeyPairsCommand", async () => {
    await callQueryFn(ec2KeyPairsQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("ec2KeyPairQueryOptions sends DescribeKeyPairsCommand with ID", async () => {
    await callQueryFn(ec2KeyPairQueryOptions("kp-abc"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      KeyPairIds: ["kp-abc"],
    })
  })

  it("ec2VolumesQueryOptions sends DescribeVolumesCommand", async () => {
    await callQueryFn(ec2VolumesQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
  })

  it("ec2VolumeQueryOptions sends DescribeVolumesCommand with ID", async () => {
    await callQueryFn(ec2VolumeQueryOptions("vol-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VolumeIds: ["vol-1"],
    })
  })

  it("ec2SnapshotsQueryOptions sends DescribeSnapshotsCommand", async () => {
    await callQueryFn(ec2SnapshotsQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
  })

  it("ec2SnapshotQueryOptions sends DescribeSnapshotsCommand with ID", async () => {
    await callQueryFn(ec2SnapshotQueryOptions("snap-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SnapshotIds: ["snap-1"],
    })
  })

  it("ec2VpcsQueryOptions sends DescribeVpcsCommand", async () => {
    await callQueryFn(ec2VpcsQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
  })

  it("ec2VpcQueryOptions sends DescribeVpcsCommand with ID", async () => {
    await callQueryFn(ec2VpcQueryOptions("vpc-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      VpcIds: ["vpc-1"],
    })
  })

  it("ec2SubnetsQueryOptions sends DescribeSubnetsCommand", async () => {
    await callQueryFn(ec2SubnetsQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
  })

  it("ec2SubnetQueryOptions sends DescribeSubnetsCommand with ID", async () => {
    await callQueryFn(ec2SubnetQueryOptions("subnet-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SubnetIds: ["subnet-1"],
    })
  })

  it("ec2AvailabilityZonesQueryOptions sends DescribeAvailabilityZonesCommand", async () => {
    await callQueryFn(ec2AvailabilityZonesQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
  })

  it("ec2RegionsQueryOptions sends DescribeRegionsCommand", async () => {
    await callQueryFn(ec2RegionsQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
  })

  it("ec2ImagesQueryOptions sends DescribeImagesCommand", async () => {
    await callQueryFn(ec2ImagesQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
  })

  it("ec2InstanceTypesQueryOptions sends DescribeInstanceTypesCommand with filter", async () => {
    await callQueryFn(ec2InstanceTypesQueryOptions)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Filters: [{ Name: "capacity", Values: ["true"] }],
    })
  })

  it("ec2PlacementGroupsQueryOptions sends DescribePlacementGroupsCommand", async () => {
    await callQueryFn(ec2PlacementGroupsQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("ec2PlacementGroupQueryOptions sends DescribePlacementGroupsCommand with ID", async () => {
    await callQueryFn(ec2PlacementGroupQueryOptions("pg-123"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupIds: ["pg-123"],
    })
  })

  it("ec2SecurityGroupsQueryOptions sends DescribeSecurityGroupsCommand", async () => {
    await callQueryFn(ec2SecurityGroupsQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("ec2SecurityGroupQueryOptions sends DescribeSecurityGroupsCommand with ID", async () => {
    await callQueryFn(ec2SecurityGroupQueryOptions("sg-123"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupIds: ["sg-123"],
    })
  })

  it("ec2IamInstanceProfileAssociationsQueryOptions filters by instance-id", async () => {
    await callQueryFn(ec2IamInstanceProfileAssociationsQueryOptions("i-123"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Filters: [{ Name: "instance-id", Values: ["i-123"] }],
    })
  })
})
