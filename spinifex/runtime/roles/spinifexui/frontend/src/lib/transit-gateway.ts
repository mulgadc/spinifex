import type {
  Subnet,
  TransitGateway,
  TransitGatewayVpcAttachment,
} from "@aws-sdk/client-ec2"

import { shellSingleQuote } from "@/components/cli-command-panel"

// A gateway or attachment already deleting or deleted has nothing left to delete.
export function isGoneState(state: string | undefined): boolean {
  return state === "deleting" || state === "deleted"
}

// The gateways a VPC's route table can target: CreateRoute takes an available
// gateway with a pending or available attachment of that VPC.
export function routableTransitGateways(
  gateways: readonly TransitGateway[],
  attachments: readonly TransitGatewayVpcAttachment[],
  vpcId: string | undefined,
): TransitGateway[] {
  const attached = new Set(
    attachments.flatMap((att) =>
      att.VpcId === vpcId &&
      (att.State === "pending" || att.State === "available")
        ? [att.TransitGatewayId]
        : [],
    ),
  )

  return gateways.filter(
    (tgw) => tgw.State === "available" && attached.has(tgw.TransitGatewayId),
  )
}

// The CLI's --tag-specifications value for a Name tag, quoted for the shell.
export function nameTagSpecificationArg(
  resourceType: "transit-gateway" | "transit-gateway-attachment",
  name: string,
): string {
  return shellSingleQuote(
    JSON.stringify([
      { ResourceType: resourceType, Tags: [{ Key: "Name", Value: name }] },
    ]),
  )
}

// An option reads "enable" or "disable" on the wire.
export function enabledLabel(value: string | undefined): string | undefined {
  if (value === undefined) {
    return undefined
  }

  return value === "enable" ? "Enabled" : "Disabled"
}

// An attachment takes at most one subnet in each Availability Zone, so a newly
// chosen subnet replaces the one already chosen in its zone.
export function onePerZone(
  next: readonly string[],
  previous: readonly string[],
  subnets: readonly Subnet[],
): string[] {
  const zone = new Map(subnets.map((s) => [s.SubnetId, s.AvailabilityZone]))
  const added = next.find((id) => !previous.includes(id))

  if (added === undefined) {
    return [...next]
  }

  return next.filter((id) => id === added || zone.get(id) !== zone.get(added))
}
