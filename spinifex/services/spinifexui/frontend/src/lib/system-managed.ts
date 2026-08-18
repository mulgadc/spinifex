import type { Image } from "@aws-sdk/client-ec2"

// Tag key applied by Spinifex to platform-managed resources
// (HAProxy VMs, their ENIs). The value identifies the owning component
// (e.g. "elbv2"). See spinifex/docs/TAG-CONVENTIONS.md.
export const SYSTEM_MANAGED_TAG_KEY = "spinifex:managed-by"

export function isSystemManagedImage(image: Image): boolean {
  return image.Tags?.some((tag) => tag.Key === SYSTEM_MANAGED_TAG_KEY) ?? false
}

// Tag value identifying the EKS node system image (K3s server+agent),
// applied by `spx admin images import --tag spinifex:managed-by=eks`.
export const EKS_SYSTEM_IMAGE_TAG_VALUE = "eks"

export function isEksSystemImage(image: Image): boolean {
  return (
    image.Tags?.some(
      (tag) =>
        tag.Key === SYSTEM_MANAGED_TAG_KEY &&
        tag.Value === EKS_SYSTEM_IMAGE_TAG_VALUE,
    ) ?? false
  )
}

// Tag value identifying the ECS node system image (containerd + ecs-agent),
// applied by `spx admin images import --tag spinifex:managed-by=ecs`.
export const ECS_SYSTEM_IMAGE_TAG_VALUE = "ecs"

export function isEcsSystemImage(image: Image): boolean {
  return (
    image.Tags?.some(
      (tag) =>
        tag.Key === SYSTEM_MANAGED_TAG_KEY &&
        tag.Value === ECS_SYSTEM_IMAGE_TAG_VALUE,
    ) ?? false
  )
}

// Tag value identifying an RDS engine image, applied by
// `spx admin images import --name spinifex-rds-<engine>`.
export const RDS_SYSTEM_IMAGE_TAG_VALUE = "rds"

// RDS launch resolves images by these tags rather than by image name.
export const RDS_ENGINE_TAG_KEY = "engine"
export const RDS_ENGINE_VERSION_TAG_KEY = "engine-version"
export const RDS_DATA_VOLUME_CONTRACT_TAG_KEY = "rds-data-volume-contract"
export const RDS_DATA_VOLUME_CONTRACT_TAG_VALUE = "format-auth-v1"

export function isRdsSystemImage(
  image: Image,
  engine: string,
  engineVersion: string,
): boolean {
  const tags = image.Tags ?? []
  const hasTag = (key: string, value: string) =>
    tags.some((tag) => tag.Key === key && tag.Value === value)

  return (
    hasTag(SYSTEM_MANAGED_TAG_KEY, RDS_SYSTEM_IMAGE_TAG_VALUE) &&
    hasTag(RDS_ENGINE_TAG_KEY, engine) &&
    hasTag(RDS_ENGINE_VERSION_TAG_KEY, engineVersion) &&
    hasTag(RDS_DATA_VOLUME_CONTRACT_TAG_KEY, RDS_DATA_VOLUME_CONTRACT_TAG_VALUE)
  )
}

export function rdsImportCommand(engine: string): string {
  return `spx admin images import --name spinifex-rds-${engine}`
}
