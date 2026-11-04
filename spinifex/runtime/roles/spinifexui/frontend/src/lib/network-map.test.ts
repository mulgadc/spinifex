import type { Instance, RouteTable, Subnet } from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import {
  buildNetworkMap,
  layoutNetworkMap,
  MAP_BUDGET,
  MAP_TIERS,
  type NetworkInputs,
} from "./network-map"

const VPC = "vpc-1"

function subnet(n: number, az = "ap-southeast-2a"): Subnet {
  return {
    SubnetId: `subnet-${n}`,
    VpcId: VPC,
    CidrBlock: `10.0.${n}.0/24`,
    AvailabilityZone: az,
  }
}

function instance(n: number, subnetId: string): Instance {
  return {
    InstanceId: `i-${String(n).padStart(4, "0")}`,
    VpcId: VPC,
    SubnetId: subnetId,
    PrivateIpAddress: `10.0.0.${n}`,
    State: { Name: "running" },
  }
}

const PUBLIC_TABLE: RouteTable = {
  RouteTableId: "rtb-main",
  VpcId: VPC,
  Associations: [{ Main: true }],
  Routes: [
    { DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" },
    { DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1" },
  ],
}

const PRIVATE_TABLE: RouteTable = {
  RouteTableId: "rtb-private",
  VpcId: VPC,
  Associations: [{ SubnetId: "subnet-2" }],
  Routes: [
    { DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" },
    { DestinationCidrBlock: "0.0.0.0/0", NatGatewayId: "nat-1" },
    { DestinationIpv6CidrBlock: "::/0", GatewayId: "igw-1" },
  ],
}

function inputs(over: Partial<NetworkInputs> = {}): NetworkInputs {
  return {
    vpc: { VpcId: VPC, CidrBlock: "10.0.0.0/16" },
    subnets: [subnet(1), subnet(2, "ap-southeast-2b")],
    routeTables: [PUBLIC_TABLE, PRIVATE_TABLE],
    internetGateways: [
      { InternetGatewayId: "igw-1", Attachments: [{ VpcId: VPC }] },
      { InternetGatewayId: "igw-other", Attachments: [{ VpcId: "vpc-2" }] },
    ],
    natGateways: [
      {
        NatGatewayId: "nat-1",
        VpcId: VPC,
        SubnetId: "subnet-1",
        State: "available",
        NatGatewayAddresses: [{ AllocationId: "eipalloc-nat" }],
      },
      { NatGatewayId: "nat-gone", VpcId: VPC, State: "deleted" },
    ],
    addresses: [
      { AllocationId: "eipalloc-nat", PublicIp: "203.0.113.10" },
      {
        AllocationId: "eipalloc-web",
        PublicIp: "203.0.113.11",
        InstanceId: "i-0001",
      },
      { AllocationId: "eipalloc-free", PublicIp: "203.0.113.12" },
    ],
    instances: [
      instance(1, "subnet-1"),
      instance(2, "subnet-2"),
      { ...instance(3, "subnet-2"), State: { Name: "terminated" } },
    ],
    loadBalancers: [
      {
        LoadBalancerArn: "arn:lb/web",
        LoadBalancerName: "web",
        VpcId: VPC,
        Type: "application",
        AvailabilityZones: [{ SubnetId: "subnet-1" }, { SubnetId: "subnet-2" }],
      },
      { LoadBalancerArn: "arn:lb/elsewhere", VpcId: "vpc-2" },
    ],
    // As our DescribeDBInstances reports it: the group's name and VPC, and no
    // subnet list.
    dbInstances: [
      {
        DBInstanceIdentifier: "orders",
        Engine: "postgres",
        DBSubnetGroup: {
          DBSubnetGroupName: "default-vpc-1",
          VpcId: VPC,
          SubnetGroupStatus: "Complete",
        },
      },
      {
        DBInstanceIdentifier: "elsewhere",
        DBSubnetGroup: { VpcId: "vpc-2" },
      },
    ],
    incomplete: [],
    failed: [],
    ...over,
  }
}

const VPC_FOCUS = { kind: "vpc", id: VPC } as const

function ids(map: ReturnType<typeof buildNetworkMap>) {
  return map.cards.map((c) => c.id).toSorted()
}

describe("buildNetworkMap", () => {
  it("places each resource in its tier and leaves other VPCs out", () => {
    const map = buildNetworkMap(inputs(), VPC_FOCUS)
    const tierOf = (id: string) => map.cards.find((c) => c.id === id)?.tier

    expect(ids(map)).toStrictEqual([
      "eip:eipalloc-nat",
      "eip:eipalloc-web",
      "igw:igw-1",
      "instance:i-0001",
      "instance:i-0002",
      "internet",
      "lb:arn:lb/web",
      "nat:nat-1",
      "rds:orders",
      "subnet:subnet-1",
      "subnet:subnet-2",
      "vpc:vpc-1",
    ])
    expect(tierOf("igw:igw-1")).toBe("gateways")
    expect(tierOf("eip:eipalloc-web")).toBe("gateways")
    expect(tierOf("subnet:subnet-2")).toBe("subnets")
    expect(tierOf("lb:arn:lb/web")).toBe("resources")
    expect(map.omitted).toBe(0)
  })

  it("draws a database at VPC level, naming its subnet group", () => {
    const map = buildNetworkMap(inputs(), VPC_FOCUS)
    const db = map.cards.find((c) => c.id === "rds:orders")

    expect(db?.tier).toBe("vpc")
    expect(db?.details).toContain("subnet group default-vpc-1")
    expect(map.links.filter((l) => l.target === "rds:orders")).toStrictEqual([
      expect.objectContaining({ source: "vpc:vpc-1" }),
    ])
  })

  it("draws one labelled line per route to a gateway", () => {
    const map = buildNetworkMap(inputs(), VPC_FOCUS)

    const routes = map.links
      .filter((l) => l.label)
      .map((l) => `${l.target} ${l.label} ${l.source}`)
      .toSorted()

    expect(routes).toStrictEqual([
      "subnet:subnet-1 0.0.0.0/0 igw:igw-1",
      "subnet:subnet-2 0.0.0.0/0 nat:nat-1",
      "subnet:subnet-2 ::/0 igw:igw-1",
    ])
  })

  it("names the route table each subnet routes by", () => {
    const map = buildNetworkMap(inputs(), VPC_FOCUS)

    expect(
      map.cards.find((c) => c.id === "subnet:subnet-1")?.details,
    ).toContain("rtb-main")
    expect(
      map.cards.find((c) => c.id === "subnet:subnet-2")?.details,
    ).toContain("rtb-private")
  })

  it("joins a load balancer to every subnet it is in", () => {
    const map = buildNetworkMap(inputs(), VPC_FOCUS)

    expect(
      map.links
        .filter((l) => l.target === "lb:arn:lb/web")
        .map((l) => l.source)
        .toSorted(),
    ).toStrictEqual(["subnet:subnet-1", "subnet:subnet-2"])
  })

  it("highlights the VPC and dims nothing, since it holds the whole map", () => {
    const map = buildNetworkMap(inputs(), VPC_FOCUS)

    expect(
      map.cards.filter((c) => c.highlighted).map((c) => c.id),
    ).toStrictEqual(["vpc:vpc-1"])
    expect(map.cards.some((c) => c.dimmed)).toBeFalsy()
  })

  it("passes on the resource types it could not read in full", () => {
    const map = buildNetworkMap(
      inputs({ incomplete: ["instances"], failed: ["dbInstances"] }),
      VPC_FOCUS,
    )

    expect(map.incomplete).toStrictEqual(["instances"])
    expect(map.failed).toStrictEqual(["dbInstances"])
  })

  describe("over budget", () => {
    // 60 subnets each routing three destinations, 200 instances: far past both
    // limits on cards and on lines.
    const crowded = inputs({
      subnets: Array.from({ length: 60 }, (_, n) =>
        subnet(n + 1, `ap-southeast-2${"abc"[n % 3]}`),
      ),
      routeTables: [
        {
          ...PUBLIC_TABLE,
          Routes: [
            { DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1" },
            { DestinationCidrBlock: "198.51.100.0/24", GatewayId: "igw-1" },
            { DestinationIpv6CidrBlock: "::/0", GatewayId: "igw-1" },
          ],
        },
      ],
      instances: Array.from({ length: 200 }, (_, n) =>
        instance(n + 1, `subnet-${(n % 60) + 1}`),
      ),
    })

    it("never passes either limit", () => {
      for (const budget of [MAP_BUDGET, { cards: 30, edges: 400 }]) {
        const map = buildNetworkMap(crowded, VPC_FOCUS, budget)

        expect(map.cards.length).toBeLessThanOrEqual(budget.cards)
        expect(map.links.length).toBeLessThanOrEqual(budget.edges)
      }

      const lines = buildNetworkMap(crowded, VPC_FOCUS, {
        cards: 400,
        edges: 50,
      })

      expect(lines.links.length).toBeLessThanOrEqual(50)
    })

    it("keeps the frame and collapses the rest into one card per tier", () => {
      const map = buildNetworkMap(crowded, VPC_FOCUS)
      const more = map.cards.filter((c) => c.kind === "more")

      expect(ids(map)).toContain("internet")
      expect(ids(map)).toContain("vpc:vpc-1")
      expect(ids(map)).toContain("igw:igw-1")
      expect(more.length).toBeGreaterThan(0)
      expect(more.every((c) => MAP_TIERS.includes(c.tier))).toBeTruthy()
      expect(new Set(more.map((c) => c.tier)).size).toBe(more.length)

      const collapsed = more.reduce(
        (sum, c) => sum + Number(/\d+/u.exec(c.label)?.[0]),
        0,
      )

      expect(collapsed).toBe(map.omitted)
      expect(map.omitted + map.cards.length - more.length).toBe(
        buildNetworkMap(crowded, VPC_FOCUS, { cards: 1000, edges: 2000 }).cards
          .length,
      )
    })

    it("joins each collapsed card by one line and no other", () => {
      const map = buildNetworkMap(crowded, VPC_FOCUS)

      for (const card of map.cards.filter((c) => c.kind === "more")) {
        expect(
          map.links.filter((l) => l.source === card.id || l.target === card.id),
        ).toHaveLength(1)
      }
    })

    it("holds the budget at the most the queries can read", () => {
      // Five pages of 100 for each paged type.
      const largest = inputs({
        subnets: Array.from({ length: 500 }, (_, n) => subnet(n + 1)),
        instances: Array.from({ length: 500 }, (_, n) =>
          instance(n + 1, `subnet-${n + 1}`),
        ),
      })

      const map = buildNetworkMap(largest, VPC_FOCUS)

      expect(map.cards.length).toBeLessThanOrEqual(MAP_BUDGET.cards)
      expect(map.links.length).toBeLessThanOrEqual(MAP_BUDGET.edges)
      expect(map.omitted).toBeGreaterThan(800)
    })

    it("only draws lines between cards it placed", () => {
      const map = buildNetworkMap(crowded, VPC_FOCUS)
      const placed = new Set(map.cards.map((c) => c.id))

      expect(
        map.links.every((l) => placed.has(l.source) && placed.has(l.target)),
      ).toBeTruthy()
    })
  })
})

describe("layoutNetworkMap", () => {
  it("columns subnets by zone and stacks each subnet's resources under it", () => {
    const map = buildNetworkMap(inputs(), VPC_FOCUS)
    const places = layoutNetworkMap(map)
    const at = (id: string) => places.get(id)

    expect(at("subnet:subnet-1")?.x).toBeLessThan(at("subnet:subnet-2")?.x ?? 0)
    expect(at("instance:i-0002")?.x).toBe(at("subnet:subnet-2")?.x)
    expect(at("internet")?.y).toBeLessThan(at("igw:igw-1")?.y ?? 0)
    expect(at("igw:igw-1")?.y).toBeLessThan(at("vpc:vpc-1")?.y ?? 0)
    expect(at("vpc:vpc-1")?.y).toBeLessThan(at("subnet:subnet-1")?.y ?? 0)
    expect(places.size).toBe(map.cards.length)
  })
})
