import type {
  VpcPeeringConnection,
  VpcPeeringConnectionVpcInfo,
} from "@aws-sdk/client-ec2"

// The states a connection can still carry traffic or be routed to from.
export const LIVE_PEERING_STATES = [
  "pending-acceptance",
  "provisioning",
  "active",
] as const

export function isLivePeering(pcx: VpcPeeringConnection): boolean {
  return LIVE_PEERING_STATES.some((state) => state === pcx.Status?.Code)
}

export type PeeringSide = "requester" | "accepter"

// The side the caller's account owns; the requester wins within one account.
export function ownSide(
  pcx: VpcPeeringConnection,
  accountId: string | undefined,
): PeeringSide | undefined {
  if (accountId === undefined) {
    return undefined
  }

  if (pcx.RequesterVpcInfo?.OwnerId === accountId) {
    return "requester"
  }

  return pcx.AccepterVpcInfo?.OwnerId === accountId ? "accepter" : undefined
}

export function sideInfo(
  pcx: VpcPeeringConnection,
  side: PeeringSide,
): VpcPeeringConnectionVpcInfo | undefined {
  return side === "requester" ? pcx.RequesterVpcInfo : pcx.AccepterVpcInfo
}

// Only the accepter's owner answers a pending request.
export function canAnswer(
  pcx: VpcPeeringConnection,
  accountId: string | undefined,
): boolean {
  return (
    pcx.Status?.Code === "pending-acceptance" &&
    accountId !== undefined &&
    pcx.AccepterVpcInfo?.OwnerId === accountId
  )
}

export function canDelete(pcx: VpcPeeringConnection): boolean {
  return isLivePeering(pcx)
}

// The other end of a connection, seen from one of its VPCs.
export function peerOf(
  pcx: VpcPeeringConnection,
  vpcId: string | undefined,
): VpcPeeringConnectionVpcInfo | undefined {
  return pcx.RequesterVpcInfo?.VpcId === vpcId
    ? pcx.AccepterVpcInfo
    : pcx.RequesterVpcInfo
}

export function touchesVpc(
  pcx: VpcPeeringConnection,
  vpcId: string | undefined,
): boolean {
  return (
    vpcId !== undefined &&
    (pcx.RequesterVpcInfo?.VpcId === vpcId ||
      pcx.AccepterVpcInfo?.VpcId === vpcId)
  )
}
