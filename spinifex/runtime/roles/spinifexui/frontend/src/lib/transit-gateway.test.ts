import { describe, expect, it } from "vitest"

import {
  isGoneState,
  onePerZone,
  routableTransitGateways,
} from "./transit-gateway"

const GATEWAYS = [
  { TransitGatewayId: "tgw-ready", State: "available" as const },
  { TransitGatewayId: "tgw-pending-att", State: "available" as const },
  { TransitGatewayId: "tgw-new", State: "pending" as const },
  { TransitGatewayId: "tgw-other-vpc", State: "available" as const },
  { TransitGatewayId: "tgw-detached", State: "available" as const },
  { TransitGatewayId: "tgw-none", State: "available" as const },
]

const ATTACHMENTS = [
  {
    TransitGatewayId: "tgw-ready",
    VpcId: "vpc-1",
    State: "available" as const,
  },
  {
    TransitGatewayId: "tgw-pending-att",
    VpcId: "vpc-1",
    State: "pending" as const,
  },
  { TransitGatewayId: "tgw-new", VpcId: "vpc-1", State: "pending" as const },
  {
    TransitGatewayId: "tgw-other-vpc",
    VpcId: "vpc-2",
    State: "available" as const,
  },
  {
    TransitGatewayId: "tgw-detached",
    VpcId: "vpc-1",
    State: "deleting" as const,
  },
]

describe("routableTransitGateways", () => {
  it("offers an available gateway with a live attachment of the VPC", () => {
    expect(
      routableTransitGateways(GATEWAYS, ATTACHMENTS, "vpc-1").map(
        (tgw) => tgw.TransitGatewayId,
      ),
    ).toStrictEqual(["tgw-ready", "tgw-pending-att"])
  })

  it("offers nothing to a VPC with no attachment", () => {
    expect(
      routableTransitGateways(GATEWAYS, ATTACHMENTS, "vpc-3"),
    ).toStrictEqual([])
  })
})

describe("isGoneState", () => {
  it("reads deleting and deleted as gone, and every other state as live", () => {
    expect(
      ["pending", "available", "modifying", "deleting", "deleted"].map(
        isGoneState,
      ),
    ).toStrictEqual([false, false, false, true, true])
  })
})

describe("onePerZone", () => {
  const SUBNETS = [
    { SubnetId: "subnet-a1", AvailabilityZone: "az-a" },
    { SubnetId: "subnet-a2", AvailabilityZone: "az-a" },
    { SubnetId: "subnet-b1", AvailabilityZone: "az-b" },
  ]

  it("replaces the subnet already chosen in the new subnet's zone", () => {
    expect(
      onePerZone(
        ["subnet-a1", "subnet-b1", "subnet-a2"],
        ["subnet-a1", "subnet-b1"],
        SUBNETS,
      ),
    ).toStrictEqual(["subnet-b1", "subnet-a2"])
  })

  it("keeps a subnet in another zone, and a removal as it is", () => {
    expect(
      onePerZone(["subnet-a1", "subnet-b1"], ["subnet-a1"], SUBNETS),
    ).toStrictEqual(["subnet-a1", "subnet-b1"])
    expect(
      onePerZone(["subnet-b1"], ["subnet-a1", "subnet-b1"], SUBNETS),
    ).toStrictEqual(["subnet-b1"])
  })
})
