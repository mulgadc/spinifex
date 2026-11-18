import type {
  VpcPeeringConnection,
  VpcPeeringConnectionStateReasonCode,
} from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import { canDelete, ownSide, peerOf } from "./vpc-peering"

function pcx(
  requester: string,
  accepter: string,
  code: VpcPeeringConnectionStateReasonCode = "active",
): VpcPeeringConnection {
  return {
    Status: { Code: code },
    RequesterVpcInfo: { VpcId: "vpc-a", OwnerId: requester },
    AccepterVpcInfo: { VpcId: "vpc-b", OwnerId: accepter },
  }
}

describe("ownSide", () => {
  it.each([
    ["the requester within one account", pcx("1", "1"), "1", "requester"],
    ["the accepter across accounts", pcx("1", "2"), "2", "accepter"],
    ["neither to a third account", pcx("1", "2"), "3", undefined],
    ["neither before the caller is known", pcx("1", "1"), undefined, undefined],
  ])("is %s", (_, connection, account, side) => {
    expect(ownSide(connection, account)).toBe(side)
  })
})

describe("peerOf", () => {
  it("is the other end from either VPC", () => {
    expect(peerOf(pcx("1", "2"), "vpc-a")?.VpcId).toBe("vpc-b")
    expect(peerOf(pcx("1", "2"), "vpc-b")?.VpcId).toBe("vpc-a")
  })
})

describe("canDelete", () => {
  it.each([
    ["pending-acceptance", true],
    ["provisioning", true],
    ["active", true],
    ["rejected", false],
    ["failed", false],
    ["deleted", false],
  ] as const)("is %s: %s", (code, expected) => {
    expect(canDelete(pcx("1", "2", code))).toBe(expected)
  })
})
