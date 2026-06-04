import {
  type _InstanceType,
  type PlacementStrategy,
  type Tag,
  type TagSpecification,
  type Tenancy,
  AssociateRouteTableCommand,
  AttachInternetGatewayCommand,
  AttachVolumeCommand,
  AuthorizeSecurityGroupEgressCommand,
  AuthorizeSecurityGroupIngressCommand,
  CopySnapshotCommand,
  CreateImageCommand,
  CreateInternetGatewayCommand,
  CreateKeyPairCommand,
  CreatePlacementGroupCommand,
  CreateRouteCommand,
  CreateRouteTableCommand,
  CreateSecurityGroupCommand,
  CreateSnapshotCommand,
  CreateSubnetCommand,
  CreateVolumeCommand,
  CreateVpcCommand,
  DeleteKeyPairCommand,
  DeletePlacementGroupCommand,
  DeleteSecurityGroupCommand,
  DeleteSnapshotCommand,
  DeleteSubnetCommand,
  DeleteVolumeCommand,
  DeleteVpcCommand,
  DetachVolumeCommand,
  GetConsoleOutputCommand,
  ImportKeyPairCommand,
  ModifyInstanceAttributeCommand,
  ModifySubnetAttributeCommand,
  ModifyVolumeCommand,
  RebootInstancesCommand,
  ResourceType,
  RevokeSecurityGroupEgressCommand,
  RevokeSecurityGroupIngressCommand,
  RunInstancesCommand,
  StartInstancesCommand,
  StopInstancesCommand,
  TerminateInstancesCommand,
} from "@aws-sdk/client-ec2"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getEc2Client } from "@/lib/awsClient"
import { calculateSubnetCidrs } from "@/lib/subnet-calculator"
import { ec2KeyPairsQueryOptions } from "@/queries/ec2"
import type {
  AttachVolumeFormData,
  CopySnapshotFormData,
  CreateImageParams,
  CreateInstanceParams,
  CreateKeyPairData,
  CreatePlacementGroupFormData,
  CreateSecurityGroupFormData,
  CreateSnapshotFormData,
  CreateSubnetFormData,
  CreateVolumeFormData,
  CreateVpcFormData,
  CreateVpcWizardFormData,
  DetachVolumeFormData,
  ImportKeyPairData,
  ModifyInstanceTypeFormData,
  ModifyVolumeParams,
  SecurityGroupRuleFormData,
} from "@/types/ec2"

const WHITESPACE_REGEX = /\s+/

export function useStartInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (instanceId: string) => {
      const command = new StartInstancesCommand({
        InstanceIds: [instanceId],
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "instances"] })
    },
  })
}

export function useStopInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (instanceId: string) => {
      const command = new StopInstancesCommand({
        InstanceIds: [instanceId],
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "instances"] })
    },
  })
}

export function useTerminateInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (instanceId: string) => {
      const command = new TerminateInstancesCommand({
        InstanceIds: [instanceId],
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "instances"] })
    },
  })
}

export function useRebootInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (instanceId: string) => {
      const command = new RebootInstancesCommand({
        InstanceIds: [instanceId],
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "instances"] })
    },
  })
}

export function useCreateInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateInstanceParams) => {
      const command = new RunInstancesCommand({
        ImageId: params.imageId,
        // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- AWS SDK expects _InstanceType enum
        InstanceType: params.instanceType as _InstanceType,
        KeyName: params.keyName,
        MinCount: params.count,
        MaxCount: params.count,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        SubnetId: params.subnetId || undefined,
        Placement: params.placementGroupName
          ? { GroupName: params.placementGroupName }
          : undefined,
        BlockDeviceMappings: buildBlockDeviceMappings(params),
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "instances"] })
    },
  })
}

function buildBlockDeviceMappings(params: CreateInstanceParams) {
  const {
    rootDeviceName,
    rootVolumeSize,
    rootVolumeType,
    rootDeleteOnTermination,
  } = params
  const hasOverride =
    rootVolumeSize !== undefined ||
    rootVolumeType !== undefined ||
    rootDeleteOnTermination !== undefined
  return hasOverride
    ? [
        {
          DeviceName: rootDeviceName,
          Ebs: {
            ...(rootVolumeSize !== undefined && { VolumeSize: rootVolumeSize }),
            ...(rootVolumeType !== undefined && { VolumeType: rootVolumeType }),
            ...(rootDeleteOnTermination !== undefined && {
              DeleteOnTermination: rootDeleteOnTermination,
            }),
          },
        },
      ]
    : undefined
}

export function useCreateKeyPair() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateKeyPairData) => {
      const command = new CreateKeyPairCommand({
        KeyName: params.keyName,
        KeyType: "rsa",
      })
      const response = await getEc2Client().send(command)
      if (!response.KeyMaterial) {
        // Private key is only ever returned once; missing material means it is unrecoverable.
        throw new Error(
          "Key pair was created but the API returned no private key. The key cannot be recovered — delete it and try again.",
        )
      }
      return response
    },
    onSuccess: () => {
      void queryClient.invalidateQueries(ec2KeyPairsQueryOptions)
    },
  })
}

export function useImportKeyPair() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ImportKeyPairData) => {
      // remove optional comment from ssh key as it breaks the import
      const keyParts = params.publicKeyMaterial.trim().split(WHITESPACE_REGEX)
      const cleanedKey = keyParts.slice(0, 2).join(" ")

      const command = new ImportKeyPairCommand({
        KeyName: params.keyName,
        PublicKeyMaterial: new TextEncoder().encode(cleanedKey),
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries(ec2KeyPairsQueryOptions)
    },
  })
}

export function useDeleteKeyPair() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (keyPairId: string) => {
      const command = new DeleteKeyPairCommand({
        KeyPairId: keyPairId,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries(ec2KeyPairsQueryOptions)
    },
  })
}

export function useCreateVolume() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateVolumeFormData) => {
      const command = new CreateVolumeCommand({
        Size: params.size,
        AvailabilityZone: params.availabilityZone,
        VolumeType: "gp3",
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "volumes"] })
    },
  })
}

export function useModifyVolume() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ModifyVolumeParams) => {
      const command = new ModifyVolumeCommand({
        VolumeId: params.volumeId,
        Size: params.size,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "volumes"] })
    },
  })
}

export function useDeleteVolume() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (volumeId: string) => {
      const command = new DeleteVolumeCommand({
        VolumeId: volumeId,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "volumes"] })
    },
  })
}

export function useCreateSnapshot() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateSnapshotFormData) => {
      const command = new CreateSnapshotCommand({
        VolumeId: params.volumeId,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Description: params.description || undefined,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "snapshots"] })
    },
  })
}

export function useDeleteSnapshot() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (snapshotId: string) => {
      const command = new DeleteSnapshotCommand({
        SnapshotId: snapshotId,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "snapshots"] })
    },
  })
}

export function useCopySnapshot() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CopySnapshotFormData) => {
      const command = new CopySnapshotCommand({
        SourceSnapshotId: params.sourceSnapshotId,
        SourceRegion: params.sourceRegion,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Description: params.description || undefined,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "snapshots"] })
    },
  })
}

export function useAttachVolume() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: AttachVolumeFormData) => {
      const command = new AttachVolumeCommand({
        VolumeId: params.volumeId,
        InstanceId: params.instanceId,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Device: params.device || undefined,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "volumes"] })
      void queryClient.invalidateQueries({ queryKey: ["ec2", "instances"] })
    },
  })
}

export function useDetachVolume() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: DetachVolumeFormData) => {
      const command = new DetachVolumeCommand({
        VolumeId: params.volumeId,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        InstanceId: params.instanceId || undefined,
        Force: params.force,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "volumes"] })
      void queryClient.invalidateQueries({ queryKey: ["ec2", "instances"] })
    },
  })
}

export function useModifyInstanceAttribute() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      instanceId,
      ...params
    }: ModifyInstanceTypeFormData & { instanceId: string }) => {
      const command = new ModifyInstanceAttributeCommand({
        InstanceId: instanceId,
        InstanceType: { Value: params.instanceType },
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "instances"] })
    },
  })
}

export function useGetConsoleOutput() {
  return useMutation({
    mutationFn: async (instanceId: string) => {
      const command = new GetConsoleOutputCommand({
        InstanceId: instanceId,
      })
      return await getEc2Client().send(command)
    },
  })
}

export function useCreateImage() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateImageParams) => {
      const command = new CreateImageCommand({
        InstanceId: params.instanceId,
        Name: params.name,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Description: params.description || undefined,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "images"] })
    },
  })
}

export function useCreateVpc() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateVpcFormData) => {
      const command = new CreateVpcCommand({
        CidrBlock: params.cidrBlock,
        TagSpecifications: params.name
          ? [
              {
                ResourceType: "vpc",
                Tags: [{ Key: "Name", Value: params.name }],
              },
            ]
          : undefined,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "vpcs"] })
    },
  })
}

export function useDeleteVpc() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (vpcId: string) => {
      const command = new DeleteVpcCommand({
        VpcId: vpcId,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "vpcs"] })
    },
  })
}

export function useCreateSubnet() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateSubnetFormData) => {
      const command = new CreateSubnetCommand({
        VpcId: params.vpcId,
        CidrBlock: params.cidrBlock,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        AvailabilityZone: params.availabilityZone || undefined,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "subnets"] })
    },
  })
}

export function useDeleteSubnet() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (subnetId: string) => {
      const command = new DeleteSubnetCommand({
        SubnetId: subnetId,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "subnets"] })
    },
  })
}

export function useCreatePlacementGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreatePlacementGroupFormData) => {
      const command = new CreatePlacementGroupCommand({
        GroupName: params.groupName,
        // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- AWS SDK expects PlacementStrategy enum
        Strategy: params.strategy as PlacementStrategy,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "placementGroups"],
      })
    },
  })
}

export function useDeletePlacementGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (groupName: string) => {
      const command = new DeletePlacementGroupCommand({
        GroupName: groupName,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "placementGroups"],
      })
    },
  })
}

export function useCreateSecurityGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateSecurityGroupFormData) => {
      const command = new CreateSecurityGroupCommand({
        GroupName: params.groupName,
        Description: params.description,
        VpcId: params.vpcId,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups"],
      })
    },
  })
}

export function useDeleteSecurityGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (groupId: string) => {
      const command = new DeleteSecurityGroupCommand({
        GroupId: groupId,
      })
      return await getEc2Client().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups"],
      })
    },
  })
}

export function useAuthorizeSecurityGroupIngress() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (
      params: SecurityGroupRuleFormData & { groupId: string },
    ) => {
      const command = new AuthorizeSecurityGroupIngressCommand({
        GroupId: params.groupId,
        IpPermissions: [
          {
            IpProtocol: params.ipProtocol,
            FromPort: params.fromPort,
            ToPort: params.toPort,
            IpRanges: [{ CidrIp: params.cidrIp }],
          },
        ],
      })
      return await getEc2Client().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups"],
      })
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups", params.groupId],
      })
    },
  })
}

export function useAuthorizeSecurityGroupEgress() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (
      params: SecurityGroupRuleFormData & { groupId: string },
    ) => {
      const command = new AuthorizeSecurityGroupEgressCommand({
        GroupId: params.groupId,
        IpPermissions: [
          {
            IpProtocol: params.ipProtocol,
            FromPort: params.fromPort,
            ToPort: params.toPort,
            IpRanges: [{ CidrIp: params.cidrIp }],
          },
        ],
      })
      return await getEc2Client().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups"],
      })
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups", params.groupId],
      })
    },
  })
}

export function useRevokeSecurityGroupIngress() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (
      params: SecurityGroupRuleFormData & { groupId: string },
    ) => {
      const command = new RevokeSecurityGroupIngressCommand({
        GroupId: params.groupId,
        IpPermissions: [
          {
            IpProtocol: params.ipProtocol,
            FromPort: params.fromPort,
            ToPort: params.toPort,
            IpRanges: [{ CidrIp: params.cidrIp }],
          },
        ],
      })
      return await getEc2Client().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups"],
      })
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups", params.groupId],
      })
    },
  })
}

export function useRevokeSecurityGroupEgress() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (
      params: SecurityGroupRuleFormData & { groupId: string },
    ) => {
      const command = new RevokeSecurityGroupEgressCommand({
        GroupId: params.groupId,
        IpPermissions: [
          {
            IpProtocol: params.ipProtocol,
            FromPort: params.fromPort,
            ToPort: params.toPort,
            IpRanges: [{ CidrIp: params.cidrIp }],
          },
        ],
      })
      return await getEc2Client().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups"],
      })
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "securityGroups", params.groupId],
      })
    },
  })
}

function buildTags(name: string | undefined, extraTags: Tag[]): Tag[] {
  const tags: Tag[] = []
  if (name) {
    tags.push({ Key: "Name", Value: name })
  }
  tags.push(...extraTags)
  return tags
}

function buildTagSpec(
  resourceType: ResourceType,
  name: string | undefined,
  extraTags: Tag[],
): TagSpecification[] | undefined {
  const tags = buildTags(name, extraTags)
  if (tags.length === 0) {
    return undefined
  }
  return [{ ResourceType: resourceType, Tags: tags }]
}

export interface WizardCreatedResource {
  type: string
  id: string | undefined
}

export interface WizardResult {
  vpcId: string | undefined
  created: WizardCreatedResource[]
  error?: Error
  failedStep?: string
}

export function useCreateVpcWizard() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (
      params: CreateVpcWizardFormData,
    ): Promise<WizardResult> => {
      const client = getEc2Client()
      const created: WizardCreatedResource[] = []
      const prefix = params.autoGenerateNames ? params.namePrefix : undefined
      const extraTags: Tag[] = params.tags.map((t) => ({
        Key: t.key,
        Value: t.value,
      }))

      let currentStep = "creating VPC"
      try {
        // Step 1: Create VPC
        const vpcName = prefix ? `${prefix}-vpc` : params.namePrefix
        // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- AWS SDK expects Tenancy enum
        const tenancy = params.tenancy as Tenancy
        const vpcResult = await client.send(
          new CreateVpcCommand({
            CidrBlock: params.cidrBlock,
            InstanceTenancy: tenancy,
            TagSpecifications: buildTagSpec(
              ResourceType.vpc,
              vpcName,
              extraTags,
            ),
          }),
        )
        const vpcId = vpcResult.Vpc?.VpcId
        if (!vpcId) {
          throw new Error(
            "VPC was created but no VPC ID was returned by the API",
          )
        }
        created.push({ type: "VPC", id: vpcId })

        if (params.mode === "vpc-only") {
          return { vpcId, created }
        }

        // Compute subnet CIDRs (use custom values if provided, else auto-calculate)
        const defaults = calculateSubnetCidrs(
          params.cidrBlock,
          params.publicSubnetCount,
          params.privateSubnetCount,
        )
        const publicCidrs =
          params.publicSubnetCidrs.length > 0
            ? params.publicSubnetCidrs
            : defaults.publicSubnets.map((s) => s.cidr)
        const privateCidrs =
          params.privateSubnetCidrs.length > 0
            ? params.privateSubnetCidrs
            : defaults.privateSubnets.map((s) => s.cidr)

        // Step 2: Create public subnets
        currentStep = "creating public subnets"
        const publicSubnetIds: string[] = []
        for (let i = 0; i < params.publicSubnetCount; i++) {
          const name = prefix ? `${prefix}-subnet-public-${i + 1}` : undefined
          const result = await client.send(
            new CreateSubnetCommand({
              VpcId: vpcId,
              CidrBlock: publicCidrs[i],
              TagSpecifications: buildTagSpec(
                ResourceType.subnet,
                name,
                extraTags,
              ),
            }),
          )
          const subnetId = result.Subnet?.SubnetId
          if (!subnetId) {
            throw new Error(
              `Public subnet ${i + 1} was created but no subnet ID was returned`,
            )
          }
          publicSubnetIds.push(subnetId)
          created.push({ type: "Public Subnet", id: subnetId })

          // Auto-assign public IPs on launch — CreateSubnet defaults this to
          // false, so without it instances in a "public" subnet never get a
          // routable public IP even though the IGW route is in place.
          await client.send(
            new ModifySubnetAttributeCommand({
              SubnetId: subnetId,
              MapPublicIpOnLaunch: { Value: true },
            }),
          )
        }

        // Step 3: Create private subnets
        currentStep = "creating private subnets"
        const privateSubnetIds: string[] = []
        for (let i = 0; i < params.privateSubnetCount; i++) {
          const name = prefix ? `${prefix}-subnet-private-${i + 1}` : undefined
          const result = await client.send(
            new CreateSubnetCommand({
              VpcId: vpcId,
              CidrBlock: privateCidrs[i],
              TagSpecifications: buildTagSpec(
                ResourceType.subnet,
                name,
                extraTags,
              ),
            }),
          )
          const subnetId = result.Subnet?.SubnetId
          if (!subnetId) {
            throw new Error(
              `Private subnet ${i + 1} was created but no subnet ID was returned`,
            )
          }
          privateSubnetIds.push(subnetId)
          created.push({ type: "Private Subnet", id: subnetId })
        }

        // Step 4: Create and attach internet gateway (if public subnets > 0)
        if (params.publicSubnetCount > 0) {
          currentStep = "creating internet gateway"
          const igwName = prefix ? `${prefix}-igw` : undefined
          const igwResult = await client.send(
            new CreateInternetGatewayCommand({
              TagSpecifications: buildTagSpec(
                ResourceType.internet_gateway,
                igwName,
                extraTags,
              ),
            }),
          )
          const igwId = igwResult.InternetGateway?.InternetGatewayId
          if (!igwId) {
            throw new Error(
              "Internet gateway was created but no ID was returned",
            )
          }
          created.push({ type: "Internet Gateway", id: igwId })

          // Step 5: Attach IGW to VPC
          currentStep = "attaching internet gateway to VPC"
          await client.send(
            new AttachInternetGatewayCommand({
              InternetGatewayId: igwId,
              VpcId: vpcId,
            }),
          )

          // Step 6: Create public route table
          currentStep = "creating public route table"
          const rtbPubName = prefix ? `${prefix}-rtb-public` : undefined
          const rtbPubResult = await client.send(
            new CreateRouteTableCommand({
              VpcId: vpcId,
              TagSpecifications: buildTagSpec(
                ResourceType.route_table,
                rtbPubName,
                extraTags,
              ),
            }),
          )
          const pubRtbId = rtbPubResult.RouteTable?.RouteTableId
          if (!pubRtbId) {
            throw new Error(
              "Public route table was created but no ID was returned",
            )
          }
          created.push({ type: "Public Route Table", id: pubRtbId })

          // Step 7: Add default route to IGW
          currentStep = "creating default route to internet gateway"
          await client.send(
            new CreateRouteCommand({
              RouteTableId: pubRtbId,
              DestinationCidrBlock: "0.0.0.0/0",
              GatewayId: igwId,
            }),
          )

          // Step 8: Associate public subnets with public route table
          currentStep = "associating public subnets with route table"
          for (const subnetId of publicSubnetIds) {
            await client.send(
              new AssociateRouteTableCommand({
                RouteTableId: pubRtbId,
                SubnetId: subnetId,
              }),
            )
          }
        }

        // Step 9: Create private route table (if private subnets > 0)
        if (params.privateSubnetCount > 0) {
          currentStep = "creating private route table"
          const rtbPrivName = prefix ? `${prefix}-rtb-private` : undefined
          const rtbPrivResult = await client.send(
            new CreateRouteTableCommand({
              VpcId: vpcId,
              TagSpecifications: buildTagSpec(
                ResourceType.route_table,
                rtbPrivName,
                extraTags,
              ),
            }),
          )
          const privRtbId = rtbPrivResult.RouteTable?.RouteTableId
          if (!privRtbId) {
            throw new Error(
              "Private route table was created but no ID was returned",
            )
          }
          created.push({ type: "Private Route Table", id: privRtbId })

          // Step 10: Associate private subnets with private route table
          currentStep = "associating private subnets with route table"
          for (const subnetId of privateSubnetIds) {
            await client.send(
              new AssociateRouteTableCommand({
                RouteTableId: privRtbId,
                SubnetId: subnetId,
              }),
            )
          }
        }

        return { vpcId, created }
      } catch (error) {
        return {
          vpcId: created.find((r) => r.type === "VPC")?.id,
          created,
          error: error instanceof Error ? error : new Error(String(error)),
          failedStep: `Failed while ${currentStep}`,
        }
      }
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ec2", "vpcs"] })
      void queryClient.invalidateQueries({ queryKey: ["ec2", "subnets"] })
      void queryClient.invalidateQueries({
        queryKey: ["ec2", "internetGateways"],
      })
      void queryClient.invalidateQueries({ queryKey: ["ec2", "routeTables"] })
    },
  })
}
