import type { RouteTable, Subnet } from "@aws-sdk/client-ec2"

// The table a subnet routes by: its explicit association, else its VPC's main.
export function routeTableFor(
  subnet: Subnet,
  routeTables: readonly RouteTable[],
): RouteTable | undefined {
  const { SubnetId: subnetId, VpcId: vpcId } = subnet

  return (
    (subnetId === undefined
      ? undefined
      : routeTables.find((rt) =>
          rt.Associations?.some((a) => a.SubnetId === subnetId),
        )) ??
    routeTables.find(
      (rt) => rt.VpcId === vpcId && rt.Associations?.some((a) => a.Main),
    )
  )
}

// A subnet is public when the table it routes by sends some destination to an
// internet gateway.
export function subnetIsPublic(
  subnet: Subnet,
  routeTables: readonly RouteTable[],
): boolean {
  return (
    routeTableFor(subnet, routeTables)?.Routes?.some((r) =>
      r.GatewayId?.startsWith("igw-"),
    ) ?? false
  )
}
