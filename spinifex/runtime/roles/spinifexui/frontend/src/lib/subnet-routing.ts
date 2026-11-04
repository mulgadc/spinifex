import type { RouteTable, Subnet } from "@aws-sdk/client-ec2"

// A subnet is public when the table it routes by, its explicit association or
// else its VPC's main table, sends some destination to an internet gateway.
export function subnetIsPublic(
  subnet: Subnet,
  routeTables: readonly RouteTable[],
): boolean {
  const { SubnetId: subnetId, VpcId: vpcId } = subnet

  const table =
    (subnetId === undefined
      ? undefined
      : routeTables.find((rt) =>
          rt.Associations?.some((a) => a.SubnetId === subnetId),
        )) ??
    routeTables.find(
      (rt) => rt.VpcId === vpcId && rt.Associations?.some((a) => a.Main),
    )

  return table?.Routes?.some((r) => r.GatewayId?.startsWith("igw-")) ?? false
}
