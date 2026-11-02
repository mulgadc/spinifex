import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"

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

// An RDS class is a db.-prefixed name for the EC2 type it runs on, so its size
// is that type's. Undefined when the cluster does not offer the type.
export function describeClassSize(
  dbInstanceClass: string,
  types: readonly InstanceTypeInfo[],
): string | undefined {
  const name = dbInstanceClass.replace(/^db\./, "")
  const type = types.find((t) => t.InstanceType === name)

  return type ? describeTypeSize(type) || undefined : undefined
}
