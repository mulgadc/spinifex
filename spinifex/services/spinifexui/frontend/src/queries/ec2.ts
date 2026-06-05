import {
  DescribeAddressesCommand,
  DescribeAvailabilityZonesCommand,
  DescribeIamInstanceProfileAssociationsCommand,
  DescribeImagesCommand,
  DescribeInstancesCommand,
  DescribeInstanceTypesCommand,
  DescribeInternetGatewaysCommand,
  DescribeKeyPairsCommand,
  DescribeNatGatewaysCommand,
  DescribePlacementGroupsCommand,
  DescribeRegionsCommand,
  DescribeRouteTablesCommand,
  DescribeSecurityGroupsCommand,
  DescribeSnapshotsCommand,
  DescribeSubnetsCommand,
  DescribeVolumesCommand,
  DescribeVpcsCommand,
} from "@aws-sdk/client-ec2"
import { queryOptions } from "@tanstack/react-query"

import { getEc2Client } from "@/lib/awsClient"

export const ec2AvailabilityZonesQueryOptions = queryOptions({
  queryKey: ["ec2", "availabilityZones"],
  queryFn: async () => {
    const command = new DescribeAvailabilityZonesCommand({})
    return await getEc2Client().send(command)
  },
  staleTime: 300_000,
})

export const ec2InstancesQueryOptions = queryOptions({
  queryKey: ["ec2", "instances"],
  queryFn: async () => {
    const command = new DescribeInstancesCommand({})
    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2InstanceQueryOptions = (instanceId: string) =>
  queryOptions({
    queryKey: ["ec2", "instances", instanceId],
    queryFn: async () => {
      const command = new DescribeInstancesCommand({
        InstanceIds: [instanceId],
      })
      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2IamInstanceProfileAssociationsQueryOptions = (
  instanceId: string,
) =>
  queryOptions({
    queryKey: ["ec2", "iam-instance-profile-associations", instanceId],
    queryFn: async () => {
      const command = new DescribeIamInstanceProfileAssociationsCommand({
        Filters: [{ Name: "instance-id", Values: [instanceId] }],
      })
      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2InstanceTypesQueryOptions = queryOptions({
  queryKey: ["ec2", "instances", "types"],
  queryFn: async () => {
    const command = new DescribeInstanceTypesCommand({
      Filters: [
        {
          Name: "capacity",
          Values: ["true"],
        },
      ],
    })
    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2ImagesQueryOptions = queryOptions({
  queryKey: ["ec2", "images"],
  queryFn: async () => {
    const command = new DescribeImagesCommand({})
    return await getEc2Client().send(command)
  },
})

export const ec2ImageQueryOptions = (imageId: string | undefined) =>
  queryOptions({
    queryKey: ["ec2", "images", imageId ?? "none"],
    queryFn: async () => {
      if (!imageId) {
        return { Images: [], $metadata: {} }
      }
      const command = new DescribeImagesCommand({
        ImageIds: [imageId],
      })
      return await getEc2Client().send(command)
    },
  })

export const ec2KeyPairsQueryOptions = queryOptions({
  queryKey: ["ec2", "keypairs"],
  queryFn: async () => {
    const command = new DescribeKeyPairsCommand({})
    return await getEc2Client().send(command)
  },
})

export const ec2KeyPairQueryOptions = (keyPairId: string) =>
  queryOptions({
    queryKey: ["ec2", "keypairs", keyPairId],
    queryFn: async () => {
      const command = new DescribeKeyPairsCommand({
        KeyPairIds: [keyPairId],
      })
      return await getEc2Client().send(command)
    },
  })

export const ec2RegionsQueryOptions = queryOptions({
  queryKey: ["ec2", "regions"],
  queryFn: async () => {
    const command = new DescribeRegionsCommand({})
    return await getEc2Client().send(command)
  },
  staleTime: 300_000,
})

export const ec2SubnetsQueryOptions = queryOptions({
  queryKey: ["ec2", "subnets"],
  queryFn: async () => {
    const command = new DescribeSubnetsCommand({})
    return await getEc2Client().send(command)
  },
})

export const ec2SubnetQueryOptions = (subnetId: string) =>
  queryOptions({
    queryKey: ["ec2", "subnets", subnetId],
    queryFn: async () => {
      const command = new DescribeSubnetsCommand({
        SubnetIds: [subnetId],
      })
      return await getEc2Client().send(command)
    },
  })

export const ec2SnapshotsQueryOptions = queryOptions({
  queryKey: ["ec2", "snapshots"],
  queryFn: async () => {
    const command = new DescribeSnapshotsCommand({})
    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2SnapshotQueryOptions = (snapshotId: string) =>
  queryOptions({
    queryKey: ["ec2", "snapshots", snapshotId],
    queryFn: async () => {
      const command = new DescribeSnapshotsCommand({
        SnapshotIds: [snapshotId],
      })
      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2InternetGatewaysQueryOptions = queryOptions({
  queryKey: ["ec2", "internetGateways"],
  queryFn: async () => {
    const command = new DescribeInternetGatewaysCommand({})
    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2InternetGatewayQueryOptions = (internetGatewayId: string) =>
  queryOptions({
    queryKey: ["ec2", "internetGateways", internetGatewayId],
    queryFn: async () => {
      const command = new DescribeInternetGatewaysCommand({
        InternetGatewayIds: [internetGatewayId],
      })
      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2RouteTablesQueryOptions = queryOptions({
  queryKey: ["ec2", "routeTables"],
  queryFn: async () => {
    const command = new DescribeRouteTablesCommand({})
    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2RouteTableQueryOptions = (routeTableId: string) =>
  queryOptions({
    queryKey: ["ec2", "routeTables", routeTableId],
    queryFn: async () => {
      const command = new DescribeRouteTablesCommand({
        RouteTableIds: [routeTableId],
      })
      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2VpcsQueryOptions = queryOptions({
  queryKey: ["ec2", "vpcs"],
  queryFn: async () => {
    const command = new DescribeVpcsCommand({})
    return await getEc2Client().send(command)
  },
})

export const ec2VpcQueryOptions = (vpcId: string) =>
  queryOptions({
    queryKey: ["ec2", "vpcs", vpcId],
    queryFn: async () => {
      const command = new DescribeVpcsCommand({
        VpcIds: [vpcId],
      })
      return await getEc2Client().send(command)
    },
  })

export const ec2PlacementGroupsQueryOptions = queryOptions({
  queryKey: ["ec2", "placementGroups"],
  queryFn: async () => {
    const command = new DescribePlacementGroupsCommand({})
    return await getEc2Client().send(command)
  },
})

export const ec2PlacementGroupQueryOptions = (groupId: string) =>
  queryOptions({
    queryKey: ["ec2", "placementGroups", groupId],
    queryFn: async () => {
      const command = new DescribePlacementGroupsCommand({
        GroupIds: [groupId],
      })
      return await getEc2Client().send(command)
    },
  })

export const ec2SecurityGroupsQueryOptions = queryOptions({
  queryKey: ["ec2", "securityGroups"],
  queryFn: async () => {
    const command = new DescribeSecurityGroupsCommand({})
    return await getEc2Client().send(command)
  },
})

export const ec2SecurityGroupQueryOptions = (groupId: string) =>
  queryOptions({
    queryKey: ["ec2", "securityGroups", groupId],
    queryFn: async () => {
      const command = new DescribeSecurityGroupsCommand({
        GroupIds: [groupId],
      })
      return await getEc2Client().send(command)
    },
  })

export const ec2AddressesQueryOptions = queryOptions({
  queryKey: ["ec2", "addresses"],
  queryFn: async () => {
    const command = new DescribeAddressesCommand({})
    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2AddressQueryOptions = (allocationId: string) =>
  queryOptions({
    queryKey: ["ec2", "addresses", allocationId],
    queryFn: async () => {
      const command = new DescribeAddressesCommand({
        AllocationIds: [allocationId],
      })
      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2NatGatewaysQueryOptions = queryOptions({
  queryKey: ["ec2", "natGateways"],
  queryFn: async () => {
    const command = new DescribeNatGatewaysCommand({})
    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2NatGatewayQueryOptions = (natGatewayId: string) =>
  queryOptions({
    queryKey: ["ec2", "natGateways", natGatewayId],
    queryFn: async () => {
      const command = new DescribeNatGatewaysCommand({
        NatGatewayIds: [natGatewayId],
      })
      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2VolumesQueryOptions = queryOptions({
  queryKey: ["ec2", "volumes"],
  queryFn: async () => {
    const command = new DescribeVolumesCommand({})
    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2VolumeQueryOptions = (volumeId: string) =>
  queryOptions({
    queryKey: ["ec2", "volumes", volumeId],
    queryFn: async () => {
      const command = new DescribeVolumesCommand({
        VolumeIds: [volumeId],
      })
      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })
