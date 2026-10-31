import type {
  Image,
  InstanceTypeInfo,
  KeyPairInfo,
  LaunchTemplateVersion,
  SecurityGroup,
  Subnet,
} from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import type { z } from "zod"

import {
  type Architecture,
  buildImageCatalog,
  imageArchitecture,
  instanceTypeSupports,
  preferredImage,
} from "@/lib/os-images"
import { createInstanceSchema, nextLaunchWizardName } from "@/types/ec2"

// The form's field values before the schema parses them.
export type RunInstancesFormInput = z.input<typeof createInstanceSchema>

export const DEFAULT_ROOT_DEVICE_NAME = "/dev/sda1"

// "2 vCPU · 8 GiB memory", read from DescribeInstanceTypes, which reports
// the size of every type the cluster offers.
export function describeTypeSize(type: InstanceTypeInfo): string {
  const vcpus = type.VCpuInfo?.DefaultVCpus
  const mib = type.MemoryInfo?.SizeInMiB

  return [
    vcpus === undefined ? undefined : `${vcpus} vCPU`,
    mib === undefined
      ? undefined
      : `${Number((mib / 1024).toFixed(2))} GiB memory`,
  ]
    .filter(Boolean)
    .join(" · ")
}

export function typesForArchitecture(
  types: readonly InstanceTypeInfo[],
  arch: Architecture | undefined,
): InstanceTypeInfo[] {
  return types.filter((type) => instanceTypeSupports(type, arch))
}

export function uniqueTypeNames(types: readonly InstanceTypeInfo[]): string[] {
  return [
    ...new Set(types.flatMap((type) => type.InstanceType ?? [])),
  ].toSorted()
}

export function defaultTypeName(names: readonly string[]): string | undefined {
  return names.find((name) => name.endsWith(".nano")) ?? names[0]
}

// Entries per instance type name, which the form reads as the available
// capacity for that type.
export function countInstanceTypes(types: readonly InstanceTypeInfo[]) {
  const counts: Record<string, number> = {}

  for (const type of types) {
    const typeName = type.InstanceType

    if (!typeName) {
      continue
    }

    counts[typeName] = (counts[typeName] ?? 0) + 1
  }

  return counts
}

// The first architecture any instance type on the cluster can run, so the
// form opens on a launchable pairing.
export function initialArchitecture(
  architectures: readonly Architecture[],
  types: readonly InstanceTypeInfo[],
): Architecture | undefined {
  return (
    architectures.find((candidate) =>
      types.some((type) => instanceTypeSupports(type, candidate)),
    ) ?? architectures[0]
  )
}

function subnetName(subnet: Subnet): string | undefined {
  return subnet.Tags?.find((tag) => tag.Key === "Name")?.Value
}

export function subnetLabel(subnet: Subnet): string {
  const name = subnetName(subnet)

  return name ? `${name} (${subnet.SubnetId})` : (subnet.SubnetId ?? "")
}

export function subnetSummary(subnet: Subnet): string {
  return [
    subnet.CidrBlock,
    subnet.AvailabilityZone,
    subnet.VpcId,
    subnet.MapPublicIpOnLaunch
      ? "public IP assigned on launch"
      : "private address only",
  ]
    .filter(Boolean)
    .join(" · ")
}

export function resolveSubnet(
  subnets: readonly Subnet[],
  selectedSubnetId: string | undefined,
  defaultSubnet: Subnet | undefined,
): Subnet | undefined {
  return selectedSubnetId
    ? subnets.find((s) => s.SubnetId === selectedSubnetId)
    : defaultSubnet
}

interface RootMappingDefaults {
  deviceName: string
  snapshotSize: number | undefined
}

interface ImageLike {
  RootDeviceName?: string
  BlockDeviceMappings?: { DeviceName?: string; Ebs?: { VolumeSize?: number } }[]
}

export function getRootMapping(
  image: ImageLike | undefined,
): RootMappingDefaults {
  const deviceName = image?.RootDeviceName ?? DEFAULT_ROOT_DEVICE_NAME
  const mappings = image?.BlockDeviceMappings ?? []
  const root = mappings.find((m) => m.DeviceName === deviceName) ?? mappings[0]

  return {
    deviceName,
    snapshotSize: root?.Ebs?.VolumeSize,
  }
}

export function imageRootMapping(
  images: readonly Image[],
  imageId: string | undefined,
): RootMappingDefaults {
  return getRootMapping(images.find((img) => img.ImageId === imageId))
}

// resolveTemplateVersion maps a version selector ($Default, $Latest or a
// numbered version) onto the matching describe-versions entry.
export function resolveTemplateVersion(
  versions: LaunchTemplateVersion[],
  selected: string | undefined,
): LaunchTemplateVersion | undefined {
  if (versions.length === 0) {
    return undefined
  }

  if (!selected || selected === "$Default") {
    return versions.find((v) => v.DefaultVersion) ?? versions[0]
  }

  if (selected === "$Latest") {
    return versions.toSorted(
      (a, b) => (b.VersionNumber ?? 0) - (a.VersionNumber ?? 0),
    )[0]
  }

  const num = Number(selected)

  return versions.find((v) => v.VersionNumber === num)
}

interface RunInstancesDefaultsInput {
  images: readonly Image[]
  arch: Architecture | undefined
  archTypeNames: readonly string[]
  keyPairs: readonly KeyPairInfo[]
  securityGroups: readonly SecurityGroup[]
  launchTemplateId: string | undefined
  launchTemplateVersion: string | undefined
}

export function runInstancesDefaults({
  images,
  arch,
  archTypeNames,
  keyPairs,
  securityGroups,
  launchTemplateId,
  launchTemplateVersion,
}: RunInstancesDefaultsInput): RunInstancesFormInput {
  const defaultImageId = preferredImage(
    buildImageCatalog(images, arch),
  )?.ImageId

  return {
    count: 1,
    imageId: defaultImageId ?? "",
    keyName: keyPairs[0]?.KeyName ?? "",
    instanceType: defaultTypeName(archTypeNames) ?? "",
    launchTemplateId,
    launchTemplateVersion: launchTemplateVersion ?? "$Default",
    rootDeviceName: imageRootMapping(images, defaultImageId).deviceName,
    securityGroupMode: "create",
    securityGroupIds: [],
    newSgName: nextLaunchWizardName(
      securityGroups.map((sg) => sg.GroupName ?? ""),
    ),
    newSgDescription: "Created by launch wizard",
    allowSsh: true,
    allowHttp: false,
    allowHttps: false,
    ruleSource: "anywhere",
    customCidr: "",
  }
}

export function runInstancesResolver(
  images: readonly Image[],
  allInstanceTypes: readonly InstanceTypeInfo[],
  instanceTypeCounts: Record<string, number>,
) {
  return zodResolver(
    createInstanceSchema
      .refine(
        (data) =>
          !data.instanceType ||
          data.count <= (instanceTypeCounts[data.instanceType] ?? 1),
        {
          message: "Cannot exceed available capacity",
          path: ["count"],
        },
      )
      .superRefine((data, ctx) => {
        const image = images.find((img) => img.ImageId === data.imageId)
        const minSize = getRootMapping(image).snapshotSize

        if (
          data.rootVolumeSize !== undefined &&
          minSize !== undefined &&
          data.rootVolumeSize < minSize
        ) {
          ctx.addIssue({
            code: "custom",
            message: `Volume size must be at least ${minSize} GiB (AMI snapshot size)`,
            path: ["rootVolumeSize"],
          })
        }

        const imageArch = image && imageArchitecture(image)

        const type = allInstanceTypes.find(
          (t) => t.InstanceType === data.instanceType,
        )

        if (
          !data.launchTemplateId &&
          type &&
          imageArch &&
          !instanceTypeSupports(type, imageArch)
        ) {
          ctx.addIssue({
            code: "custom",
            message: `${data.instanceType} cannot run a ${imageArch} image`,
            path: ["instanceType"],
          })
        }
      }),
  )
}
