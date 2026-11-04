import type { RouteTable, Subnet } from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import { subnetIsPublic } from "./subnet-routing"

const SUBNET: Subnet = { SubnetId: "subnet-a", VpcId: "vpc-1" }

const MAIN_TO_IGW: RouteTable = {
  RouteTableId: "rtb-main",
  VpcId: "vpc-1",
  Associations: [{ Main: true }],
  Routes: [
    { DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" },
    { DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1" },
  ],
}

const EXPLICIT_TO_NAT: RouteTable = {
  RouteTableId: "rtb-private",
  VpcId: "vpc-1",
  Associations: [{ SubnetId: "subnet-a" }],
  Routes: [
    { DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" },
    { DestinationCidrBlock: "0.0.0.0/0", NatGatewayId: "nat-1" },
  ],
}

describe("subnetIsPublic", () => {
  it("is public through the VPC's main table when it has no association", () => {
    expect(subnetIsPublic(SUBNET, [MAIN_TO_IGW])).toBeTruthy()
  })

  it("follows the explicit association over the main table", () => {
    expect(subnetIsPublic(SUBNET, [MAIN_TO_IGW, EXPLICIT_TO_NAT])).toBeFalsy()
  })

  it("is public through an explicit table with an internet gateway route", () => {
    const explicit: RouteTable = {
      ...MAIN_TO_IGW,
      RouteTableId: "rtb-public",
      Associations: [{ SubnetId: "subnet-a" }],
    }

    const privateMain: RouteTable = {
      ...EXPLICIT_TO_NAT,
      Associations: [{ Main: true }],
    }

    expect(subnetIsPublic(SUBNET, [privateMain, explicit])).toBeTruthy()
  })

  it("ignores another VPC's main table", () => {
    expect(
      subnetIsPublic(SUBNET, [{ ...MAIN_TO_IGW, VpcId: "vpc-2" }]),
    ).toBeFalsy()
  })

  it("is private with no table to route by", () => {
    expect(subnetIsPublic(SUBNET, [])).toBeFalsy()
  })
})
