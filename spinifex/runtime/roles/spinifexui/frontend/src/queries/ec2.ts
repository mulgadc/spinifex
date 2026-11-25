import {
  DescribeAddressesCommand,
  DescribeAvailabilityZonesCommand,
  DescribeIamInstanceProfileAssociationsCommand,
  DescribeImagesCommand,
  DescribeInstancesCommand,
  DescribeInstanceTypesCommand,
  DescribeInternetGatewaysCommand,
  DescribeKeyPairsCommand,
  DescribeLaunchTemplatesCommand,
  DescribeLaunchTemplateVersionsCommand,
  DescribeNatGatewaysCommand,
  DescribePlacementGroupsCommand,
  DescribePrefixListsCommand,
  DescribeRegionsCommand,
  DescribeRouteTablesCommand,
  DescribeSecurityGroupsCommand,
  DescribeSnapshotsCommand,
  DescribeSubnetsCommand,
  DescribeTransitGatewaysCommand,
  DescribeTransitGatewayVpcAttachmentsCommand,
  DescribeVolumesCommand,
  DescribeVpcEndpointServicesCommand,
  DescribeVpcEndpointsCommand,
  DescribeVpcPeeringConnectionsCommand,
  DescribeVpcsCommand,
  GetConsoleOutputCommand,
} from "@aws-sdk/client-ec2"
import { queryOptions } from "@tanstack/react-query"

import { getEc2Client } from "@/lib/aws-client"

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

// Fetched on request, not on mount: the log can be large and is read rarely.
// Cached by instance, so the output survives moving from panel to page.
export const ec2ConsoleOutputQueryOptions = (instanceId: string) =>
  queryOptions({
    queryKey: ["ec2", "consoleOutput", instanceId],
    queryFn: async () => {
      const command = new GetConsoleOutputCommand({ InstanceId: instanceId })
      const result = await getEc2Client().send(command)

      return result.Output ? atob(result.Output) : ""
    },
    enabled: false,
    staleTime: Number.POSITIVE_INFINITY,
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

// The admissible instance types, read once when a panel that needs them opens.
// This is a catalogue: it changes when a node joins or leaves, not as instances
// come and go, so it is not polled. The free-slot figure is a separate query.
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
  staleTime: 5 * 60 * 1000,
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

export const ec2VpcPeeringConnectionsQueryOptions = queryOptions({
  queryKey: ["ec2", "vpcPeeringConnections"],
  queryFn: async () => {
    const command = new DescribeVpcPeeringConnectionsCommand({})

    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2VpcPeeringConnectionQueryOptions = (connectionId: string) =>
  queryOptions({
    queryKey: ["ec2", "vpcPeeringConnections", connectionId],
    queryFn: async () => {
      const command = new DescribeVpcPeeringConnectionsCommand({
        VpcPeeringConnectionIds: [connectionId],
      })

      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2TransitGatewaysQueryOptions = queryOptions({
  queryKey: ["ec2", "transitGateways"],
  queryFn: async () => {
    const command = new DescribeTransitGatewaysCommand({})

    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2TransitGatewayQueryOptions = (transitGatewayId: string) =>
  queryOptions({
    queryKey: ["ec2", "transitGateways", transitGatewayId],
    queryFn: async () => {
      const command = new DescribeTransitGatewaysCommand({
        TransitGatewayIds: [transitGatewayId],
      })

      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2TransitGatewayVpcAttachmentsQueryOptions = queryOptions({
  queryKey: ["ec2", "transitGatewayVpcAttachments"],
  queryFn: async () => {
    const command = new DescribeTransitGatewayVpcAttachmentsCommand({})

    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

// The attachments a route table's VPC can route through, for its target picker.
export const ec2VpcLiveTransitGatewayAttachmentsQueryOptions = (
  vpcId: string,
) =>
  queryOptions({
    queryKey: ["ec2", "transitGatewayVpcAttachments", "vpc", vpcId],
    queryFn: async () => {
      const command = new DescribeTransitGatewayVpcAttachmentsCommand({
        Filters: [
          { Name: "vpc-id", Values: [vpcId] },
          { Name: "state", Values: ["pending", "available"] },
        ],
      })

      return await getEc2Client().send(command)
    },
  })

export const ec2GatewayTransitGatewayAttachmentsQueryOptions = (
  transitGatewayId: string,
) =>
  queryOptions({
    queryKey: ["ec2", "transitGatewayVpcAttachments", "tgw", transitGatewayId],
    queryFn: async () => {
      const command = new DescribeTransitGatewayVpcAttachmentsCommand({
        Filters: [{ Name: "transit-gateway-id", Values: [transitGatewayId] }],
      })

      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2TransitGatewaysByIdQueryOptions = (
  transitGatewayIds: readonly string[],
) =>
  queryOptions({
    queryKey: ["ec2", "transitGateways", "ids", ...transitGatewayIds],
    queryFn: async () => {
      const command = new DescribeTransitGatewaysCommand({
        TransitGatewayIds: [...transitGatewayIds],
      })

      return await getEc2Client().send(command)
    },
  })

export const ec2TransitGatewayVpcAttachmentQueryOptions = (
  attachmentId: string,
) =>
  queryOptions({
    queryKey: ["ec2", "transitGatewayVpcAttachments", attachmentId],
    queryFn: async () => {
      const command = new DescribeTransitGatewayVpcAttachmentsCommand({
        TransitGatewayAttachmentIds: [attachmentId],
      })

      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

export const ec2VpcEndpointsQueryOptions = queryOptions({
  queryKey: ["ec2", "vpcEndpoints"],
  queryFn: async () => {
    const command = new DescribeVpcEndpointsCommand({})

    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2VpcEndpointQueryOptions = (endpointId: string) =>
  queryOptions({
    queryKey: ["ec2", "vpcEndpoints", endpointId],
    queryFn: async () => {
      const command = new DescribeVpcEndpointsCommand({
        VpcEndpointIds: [endpointId],
      })

      return await getEc2Client().send(command)
    },
    refetchInterval: 5000,
  })

// The gateway services a VPC can reach. The list is empty until the formation
// offers one, and a listed service says whether it takes a custom policy.
export const ec2GatewayEndpointServicesQueryOptions = queryOptions({
  queryKey: ["ec2", "vpcEndpointServices", "Gateway"],
  queryFn: async () => {
    const command = new DescribeVpcEndpointServicesCommand({
      Filters: [{ Name: "service-type", Values: ["Gateway"] }],
    })

    return await getEc2Client().send(command)
  },
  staleTime: 60_000,
})

// The AWS-managed prefix list named after a service, which its routes target.
export const ec2ServicePrefixListQueryOptions = (serviceName: string) =>
  queryOptions({
    queryKey: ["ec2", "prefixLists", serviceName],
    queryFn: async () => {
      const command = new DescribePrefixListsCommand({
        Filters: [{ Name: "prefix-list-name", Values: [serviceName] }],
      })

      return await getEc2Client().send(command)
    },
    staleTime: 300_000,
  })

export const ec2LaunchTemplatesQueryOptions = queryOptions({
  queryKey: ["ec2", "launchTemplates"],
  queryFn: async () => {
    const command = new DescribeLaunchTemplatesCommand({})

    return await getEc2Client().send(command)
  },
  refetchInterval: 5000,
})

export const ec2LaunchTemplateVersionsQueryOptions = (
  launchTemplateId: string,
) =>
  queryOptions({
    queryKey: ["ec2", "launchTemplates", launchTemplateId, "versions"],
    queryFn: async () => {
      const command = new DescribeLaunchTemplateVersionsCommand({
        LaunchTemplateId: launchTemplateId,
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
