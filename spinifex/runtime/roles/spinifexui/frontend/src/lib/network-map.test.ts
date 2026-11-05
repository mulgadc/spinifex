import type { Instance, RouteTable, Subnet } from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import {
  buildNetworkMap,
  CARD_WIDTH,
  internetReach,
  layoutNetworkMap,
  MAP_BUDGET,
  MAP_TIERS,
  type MapFocus,
  type NetworkInputs,
  scaleLayout,
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
      .filter((l) => l.label && !l.heavy)
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

    it("holds the budget when every subnet shares many routes to one gateway", () => {
      // 500 subnets each drawing 100 routes put 50,000 lines on the gateway.
      const shared: RouteTable = {
        ...PUBLIC_TABLE,
        Routes: Array.from({ length: 100 }, (_, n) => ({
          DestinationCidrBlock: `198.51.${n}.0/24`,
          GatewayId: "igw-1",
        })),
      }

      const map = buildNetworkMap(
        inputs({
          subnets: Array.from({ length: 500 }, (_, n) => subnet(n + 1)),
          routeTables: [shared],
        }),
        VPC_FOCUS,
      )

      expect(map.cards.length).toBeLessThanOrEqual(MAP_BUDGET.cards)
      expect(map.links.length).toBeLessThanOrEqual(MAP_BUDGET.edges)
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

function cardOf(map: ReturnType<typeof buildNetworkMap>, id: string) {
  return map.cards.find((c) => c.id === id)
}

describe("the internet path", () => {
  it("joins a NAT gateway to the internet gateway its own subnet routes to, outbound only", () => {
    const { links } = buildNetworkMap(inputs(), VPC_FOCUS)
    const egress = links.find((l) => l.target === "nat:nat-1" && l.heavy)

    expect(egress).toMatchObject({
      source: "igw:igw-1",
      label: "outbound only",
    })
  })

  it("draws no route from a main table a partial read cannot confirm", () => {
    // subnet-1 has no explicit table among those read, so its own may be unread.
    const { links } = buildNetworkMap(
      inputs({ incomplete: ["routeTables"] }),
      VPC_FOCUS,
    )

    expect(
      links.filter((l) => l.target === "subnet:subnet-1" && l.label),
    ).toStrictEqual([])
    expect(
      links.find((l) => l.target === "nat:nat-1" && l.heavy),
    ).toBeUndefined()
    expect(
      links.some((l) => l.target === "subnet:subnet-2" && l.label),
    ).toBeTruthy()
  })

  it("draws no egress for a NAT gateway whose subnet has no internet route", () => {
    const { links } = buildNetworkMap(
      inputs({
        natGateways: [
          {
            NatGatewayId: "nat-1",
            VpcId: VPC,
            SubnetId: "subnet-2",
            State: "available",
          },
        ],
        routeTables: [
          { ...PRIVATE_TABLE, Routes: PRIVATE_TABLE.Routes?.slice(0, 2) },
          PUBLIC_TABLE,
        ],
      }),
      VPC_FOCUS,
    )

    expect(links.some((l) => l.label === "outbound only")).toBeFalsy()
  })

  it("names each subnet's type from the table it routes by", () => {
    const map = buildNetworkMap(
      inputs({ routeTables: [PUBLIC_TABLE, noIgw(PRIVATE_TABLE)] }),
      VPC_FOCUS,
    )

    expect(cardOf(map, "subnet:subnet-1")?.rows).toStrictEqual([
      { label: "Type", value: "public" },
    ])
    expect(cardOf(map, "subnet:subnet-2")?.rows?.[0]?.value).toBe("private")
  })

  it("says a subnet's type is unknown when route tables did not read", () => {
    const map = buildNetworkMap(
      inputs({ routeTables: [], failed: ["routeTables"] }),
      VPC_FOCUS,
    )

    expect(cardOf(map, "subnet:subnet-1")?.rows?.[0]?.value).toBe("unknown")
  })

  it("lists an instance's public and private addresses", () => {
    const map = buildNetworkMap(
      inputs({
        instances: [
          { ...instance(1, "subnet-1"), PublicIpAddress: "203.0.113.11" },
          instance(2, "subnet-2"),
        ],
      }),
      VPC_FOCUS,
    )

    expect(cardOf(map, "instance:i-0001")?.rows).toStrictEqual([
      { label: "Public IP", value: "203.0.113.11" },
      { label: "Private IP", value: "10.0.0.1" },
    ])
    expect(cardOf(map, "instance:i-0002")?.rows?.[0]?.value).toBe("-")
  })
})

describe("internetReach", () => {
  it("is reachable with an attached internet gateway that a route uses", () => {
    expect(internetReach(inputs())).toBe("reachable")
  })

  it("is unreachable with no attached internet gateway", () => {
    expect(internetReach(inputs({ internetGateways: [] }))).toBe("unreachable")
  })

  it("is unreachable with an attached gateway that no route uses", () => {
    expect(
      internetReach(inputs({ routeTables: [PRIVATE_TABLE].map(noIgw) })),
    ).toBe("unreachable")
  })

  it("is unknown, not unreachable, when route tables were denied or cut short", () => {
    expect(
      internetReach(inputs({ routeTables: [], failed: ["routeTables"] })),
    ).toBe("unknown")
    expect(
      internetReach(inputs({ routeTables: [], incomplete: ["routeTables"] })),
    ).toBe("unknown")
    expect(
      internetReach(
        inputs({ internetGateways: [], failed: ["internetGateways"] }),
      ),
    ).toBe("unknown")
  })

  it("is drawn on the Internet card", () => {
    const map = buildNetworkMap(inputs({ internetGateways: [] }), VPC_FOCUS)

    expect(map.cards.find((c) => c.kind === "internet")?.reach).toBe(
      "unreachable",
    )
  })
})

function noIgw(table: RouteTable): RouteTable {
  return {
    ...table,
    Routes: table.Routes?.filter((r) => !r.GatewayId?.startsWith("igw-")),
  }
}

function crowdedMap() {
  return buildNetworkMap(
    inputs({
      instances: [1, 2, 3].map((n) => instance(n, "subnet-1")),
    }),
    VPC_FOCUS,
  )
}

function placeOf(places: Map<string, { x: number; y: number }>) {
  return (id: string) => places.get(id) ?? { x: Number.NaN, y: Number.NaN }
}

describe("layoutNetworkMap", () => {
  it("puts subnets side by side by zone, each subnet's resources boxed beneath it", () => {
    const map = crowdedMap()
    const { places, boxes } = layoutNetworkMap(map)
    const at = placeOf(places)

    expect(at("subnet:subnet-1").x).toBeLessThan(at("subnet:subnet-2").x)
    expect(at("internet").y).toBeLessThan(at("igw:igw-1").y)
    expect(at("igw:igw-1").y).toBeLessThan(at("vpc:vpc-1").y)
    expect(at("vpc:vpc-1").y).toBeLessThan(at("subnet:subnet-1").y)
    expect(places.size).toBe(map.cards.length)

    const box = boxes.find((b) => b.id === "box:instance:subnet-1")

    expect(box).toMatchObject({ label: "Instances" })
    expect(box?.y).toBeGreaterThan(at("subnet:subnet-1").y)
    // Two to a row: the third starts a second row under the first.
    expect(at("instance:i-0002").y).toBe(at("instance:i-0001").y)
    expect(at("instance:i-0002").x).toBeGreaterThan(at("instance:i-0001").x)
    expect(at("instance:i-0003").x).toBe(at("instance:i-0001").x)
    expect(at("instance:i-0003").y).toBeGreaterThan(at("instance:i-0001").y)

    for (const n of [1, 2, 3]) {
      const p = at(`instance:i-000${n}`)

      expect(p.x).toBeGreaterThanOrEqual(box?.x ?? Infinity)
      expect(p.x).toBeLessThan((box?.x ?? 0) + (box?.width ?? 0))
      expect(p.y).toBeLessThan((box?.y ?? 0) + (box?.height ?? 0))
    }
  })

  it("boxes load balancers apart from instances", () => {
    const { boxes } = layoutNetworkMap(crowdedMap())

    expect(boxes.map((b) => b.id).toSorted()).toStrictEqual([
      "box:instance:subnet-1",
      "box:lb:subnet-1",
    ])
  })

  it("leaves room for the outbound-only label between the gateways", () => {
    const at = placeOf(
      layoutNetworkMap(buildNetworkMap(inputs(), VPC_FOCUS)).places,
    )

    const between = at("nat:nat-1").x - at("igw:igw-1").x - CARD_WIDTH

    // Wider than "outbound only" at the label's size.
    expect(between).toBeGreaterThanOrEqual(120)
  })

  it("runs the tiers left to right across", () => {
    const { places, boxes } = layoutNetworkMap(crowdedMap(), "across")
    const at = placeOf(places)

    expect(at("internet").x).toBeLessThan(at("igw:igw-1").x)
    expect(at("igw:igw-1").x).toBeLessThan(at("vpc:vpc-1").x)
    expect(at("vpc:vpc-1").x).toBeLessThan(at("subnet:subnet-1").x)
    expect(at("subnet:subnet-1").y).toBeLessThan(at("subnet:subnet-2").y)
    expect(
      boxes.find((b) => b.id === "box:instance:subnet-1")?.x,
    ).toBeGreaterThan(at("subnet:subnet-1").x)
  })

  it("spreads every place and box by the root font's factor", () => {
    const layout = layoutNetworkMap(crowdedMap())
    const scaled = scaleLayout(layout, 1.1)
    const [box] = layout.boxes
    const [big] = scaled.boxes
    const web = layout.places.get("instance:i-0002")

    expect(big?.width).toBeCloseTo((box?.width ?? 0) * 1.1)
    expect(big?.height).toBeCloseTo((box?.height ?? 0) * 1.1)
    expect(scaled.places.get("instance:i-0002")?.x).toBeCloseTo(
      (web?.x ?? 0) * 1.1,
    )
    expect(scaleLayout(layout, 1)).toBe(layout)
  })
})

function lit(focus: MapFocus) {
  return buildNetworkMap(inputs(), focus)
    .cards.filter((c) => c.highlighted)
    .map((c) => c.id)
    .toSorted()
}

describe("a resource's focus", () => {
  it("lights a subnet and what sits in it", () => {
    expect(lit({ kind: "subnet", id: "subnet-1" })).toStrictEqual([
      "instance:i-0001",
      "lb:arn:lb/web",
      "nat:nat-1",
      "subnet:subnet-1",
    ])
  })

  it("lights an internet gateway and the subnets routing to it", () => {
    expect(lit({ kind: "igw", id: "igw-1" })).toStrictEqual([
      "igw:igw-1",
      "subnet:subnet-1",
      "subnet:subnet-2",
    ])
  })

  it("lights a NAT gateway, its subnet, its address and the subnets routing to it", () => {
    expect(lit({ kind: "nat", id: "nat-1" })).toStrictEqual([
      "eip:eipalloc-nat",
      "nat:nat-1",
      "subnet:subnet-1",
      "subnet:subnet-2",
    ])
  })

  it("lights an instance, its subnet and its address", () => {
    expect(lit({ kind: "instance", id: "i-0001" })).toStrictEqual([
      "eip:eipalloc-web",
      "instance:i-0001",
      "subnet:subnet-1",
    ])
  })

  it("lights a route table's subnets and targets, the main one serving the rest", () => {
    expect(lit({ kind: "routeTable", id: "rtb-main" })).toStrictEqual([
      "igw:igw-1",
      "subnet:subnet-1",
    ])
    expect(lit({ kind: "routeTable", id: "rtb-private" })).toStrictEqual([
      "igw:igw-1",
      "nat:nat-1",
      "subnet:subnet-2",
    ])
  })

  it("leaves the main table's fallback subnets unlit from a partial read", () => {
    const map = buildNetworkMap(inputs({ incomplete: ["routeTables"] }), {
      kind: "routeTable",
      id: "rtb-main",
    })

    expect(
      map.cards.filter((c) => c.highlighted).map((c) => c.id),
    ).toStrictEqual(["igw:igw-1"])
  })

  it("dims the rest of the VPC, and dims nothing for the VPC itself", () => {
    const map = buildNetworkMap(inputs(), { kind: "subnet", id: "subnet-1" })

    expect(cardOf(map, "vpc:vpc-1")?.dimmed).toBeTruthy()
    expect(cardOf(map, "subnet:subnet-1")?.dimmed).toBeFalsy()
    expect(
      buildNetworkMap(inputs(), VPC_FOCUS).cards.some((c) => c.dimmed),
    ).toBeFalsy()
  })

  it("keeps the instance it is drawn for when the budget is full", () => {
    const map = buildNetworkMap(
      inputs({
        instances: Array.from({ length: 500 }, (_, n) =>
          instance(n + 1, "subnet-1"),
        ),
      }),
      { kind: "instance", id: "i-0400" },
    )

    expect(cardOf(map, "instance:i-0400")?.highlighted).toBeTruthy()
    expect(map.cards.length).toBeLessThanOrEqual(MAP_BUDGET.cards)
  })

  it.each<MapFocus>([
    { kind: "subnet", id: "subnet-1" },
    { kind: "igw", id: "igw-1" },
    { kind: "routeTable", id: "rtb-main" },
  ])(
    "keeps the map's frame when $kind lights more than the budget",
    (focus) => {
      const crowd = inputs({
        subnets: Array.from({ length: 200 }, (_, n) => subnet(n + 1)),
        instances: Array.from({ length: 500 }, (_, n) =>
          instance(n + 1, "subnet-1"),
        ),
      })

      const map = buildNetworkMap(crowd, focus)
      const placed = new Set(map.cards.map((c) => c.id))

      expect(placed.has("internet")).toBeTruthy()
      expect(placed.has("vpc:vpc-1")).toBeTruthy()
      expect(
        map.links.every((l) => placed.has(l.source) && placed.has(l.target)),
      ).toBeTruthy()
      expect(map.cards.length).toBeLessThanOrEqual(MAP_BUDGET.cards)
    },
  )
})
