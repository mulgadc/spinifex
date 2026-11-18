import type { RouteTable, Subnet } from "@aws-sdk/client-ec2"

function explicitTable(
  subnetId: string | undefined,
  routeTables: readonly RouteTable[],
): RouteTable | undefined {
  return subnetId === undefined
    ? undefined
    : routeTables.find((rt) =>
        rt.Associations?.some((a) => a.SubnetId === subnetId),
      )
}

// The table a subnet routes by: its explicit association, else its VPC's main.
// From a partial read only an explicit association is sure, since the subnet's
// own table may be among those unread.
export function routeTableFor(
  subnet: Subnet,
  routeTables: readonly RouteTable[],
  complete = true,
): RouteTable | undefined {
  const { SubnetId: subnetId, VpcId: vpcId } = subnet
  const explicit = explicitTable(subnetId, routeTables)

  if (explicit || !complete) {
    return explicit
  }

  return routeTables.find(
    (rt) => rt.VpcId === vpcId && rt.Associations?.some((a) => a.Main),
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

export type SubnetType = "public" | "private" | "unknown"

// Unknown when the tables were not read, or when only some were and none is
// associated with the subnet, since its own table may be among those unread.
export function subnetType(
  subnet: Subnet,
  routeTables: readonly RouteTable[] | undefined,
  complete = true,
): SubnetType {
  if (!routeTables) {
    return "unknown"
  }

  if (!complete && !explicitTable(subnet.SubnetId, routeTables)) {
    return "unknown"
  }

  return subnetIsPublic(subnet, routeTables) ? "public" : "private"
}
