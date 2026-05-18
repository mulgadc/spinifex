import type { Image } from "@aws-sdk/client-ec2"

// Tag key applied by Spinifex to platform-managed resources
// (HAProxy VMs, their ENIs). The value identifies the owning component
// (e.g. "elbv2"). See spinifex/docs/TAG-CONVENTIONS.md.
export const SYSTEM_MANAGED_TAG_KEY = "spinifex:managed-by"

export function isSystemManagedImage(image: Image): boolean {
  return image.Tags?.some((tag) => tag.Key === SYSTEM_MANAGED_TAG_KEY) ?? false
}
