import type { IpPermission, SecurityGroup } from "@aws-sdk/client-ec2"
import { describe, expect, it } from "vitest"

import {
  buildSecurityGroupMap,
  GROUP_MAP_BUDGET,
  type GroupInputs,
  layoutSecurityGroupMap,
  protocolOf,
  ruleLabel,
} from "./security-group-map"

const GROUP_ID = "sg-0000000000000web"

function inputs(group: SecurityGroup, extra: Partial<GroupInputs> = {}) {
  return {
    group: { GroupId: GROUP_ID, GroupName: "web", ...group },
    peers: [],
    instances: [],
    loadBalancers: [],
    dbInstances: [],
    incomplete: [],
    failed: [],
    ...extra,
  }
}

const open = (rule: IpPermission): IpPermission => ({
  IpRanges: [{ CidrIp: "0.0.0.0/0" }],
  ...rule,
})

describe("ruleLabel", () => {
  it.each([
    [{ IpProtocol: "tcp", FromPort: 22, ToPort: 22 }, "tcp/22"],
    [{ IpProtocol: "udp", FromPort: 1000, ToPort: 2000 }, "udp/1000–2000"],
    [{ IpProtocol: "tcp", FromPort: 0, ToPort: 65_535 }, "tcp/all"],
    [{ IpProtocol: "6", FromPort: 443, ToPort: 443 }, "tcp/443"],
    [{ IpProtocol: "17", FromPort: 53, ToPort: 53 }, "udp/53"],
    [{ IpProtocol: "-1" }, "all traffic"],
    [{ IpProtocol: "47" }, "protocol 47"],
  ])("labels %o as %s", (rule, label) => {
    expect(ruleLabel(rule)).toBe(label)
  })

  it.each([
    [{ IpProtocol: "icmp", FromPort: 8, ToPort: -1 }, "icmp/echo-request"],
    [
      { IpProtocol: "1", FromPort: 3, ToPort: 4 },
      "icmp/destination-unreachable (4)",
    ],
    [{ IpProtocol: "icmp", FromPort: -1, ToPort: -1 }, "icmp/all"],
    [{ IpProtocol: "icmp", FromPort: 13, ToPort: 0 }, "icmp/13:0"],
    [{ IpProtocol: "icmp", FromPort: 13, ToPort: -1 }, "icmp/13:all"],
    [{ IpProtocol: "58", FromPort: 128, ToPort: -1 }, "icmpv6/echo-request"],
  ])("reads ICMP's ports as type and code: %o is %s", (rule, label) => {
    expect(ruleLabel(rule)).toBe(label)
  })

  it.each([
    ["tcp", "tcp"],
    ["6", "tcp"],
    ["17", "udp"],
    ["1", "icmp"],
    ["icmpv6", "icmp"],
    ["-1", "all"],
    ["47", "other"],
  ] as const)("draws protocol %s as %s", (IpProtocol, protocol) => {
    expect(protocolOf({ IpProtocol })).toBe(protocol)
  })
})

describe("buildSecurityGroupMap", () => {
  const group: SecurityGroup = {
    VpcId: "vpc-home",
    IpPermissions: [
      open({ IpProtocol: "tcp", FromPort: 443, ToPort: 443 }),
      {
        IpProtocol: "tcp",
        FromPort: 5432,
        ToPort: 5432,
        UserIdGroupPairs: [{ GroupId: "sg-0000000000000app" }],
      },
      { IpProtocol: "-1", UserIdGroupPairs: [{ GroupId: GROUP_ID }] },
      {
        IpProtocol: "tcp",
        FromPort: 22,
        ToPort: 22,
        PrefixListIds: [{ PrefixListId: "pl-00000000000office" }],
      },
    ],
    IpPermissionsEgress: [open({ IpProtocol: "-1" })],
  }

  const map = buildSecurityGroupMap(
    inputs(group, {
      peers: [
        { GroupId: "sg-0000000000000app", GroupName: "app", VpcId: "vpc-home" },
      ],
      instances: [
        { InstanceId: "i-in", SecurityGroups: [{ GroupId: GROUP_ID }] },
        { InstanceId: "i-out", SecurityGroups: [{ GroupId: "sg-other" }] },
      ],
      loadBalancers: [
        { LoadBalancerName: "front", SecurityGroups: [GROUP_ID] },
      ],
      dbInstances: [
        {
          DBInstanceIdentifier: "orders",
          VpcSecurityGroups: [{ VpcSecurityGroupId: GROUP_ID }],
        },
      ],
    }),
  )

  const card = (id: string) => map.cards.find((c) => c.id === id)

  it("draws each inbound source, the group, its members and each destination", () => {
    expect(map.cards.map((c) => [c.id, c.side])).toStrictEqual([
      [`group:${GROUP_ID}`, "group"],
      ["out:cidr:0.0.0.0/0", "outbound"],
      ["in:cidr:0.0.0.0/0", "inbound"],
      ["in:peer:sg-0000000000000app", "inbound"],
      ["in:prefix:pl-00000000000office", "inbound"],
      ["lb:front", "group"],
      ["instance:i-in", "group"],
      ["rds:orders", "group"],
    ])
    expect(card(`group:${GROUP_ID}`)?.highlighted).toBeTruthy()
  })

  it("names a group a rule admits, and where its members are", () => {
    expect(card("in:peer:sg-0000000000000app")).toMatchObject({
      label: "app",
      details: ["sg-0000000000000app", "members in this VPC"],
      resourceId: "sg-0000000000000app",
    })
  })

  it("draws a rule naming the group itself as one among its members, not a card", () => {
    expect(map.cards.some((c) => c.label === "")).toBeFalsy()
    expect(map.among).toStrictEqual([
      { label: "inbound (all traffic)", protocol: "all" },
    ])
  })

  it.each([
    [{ VpcId: "vpc-other" }, {}, "members in vpc-other"],
    [{}, { VpcId: "vpc-other" }, "members in vpc-other"],
    [{ VpcPeeringConnectionId: "pcx-1" }, {}, "via pcx-1"],
    [{ VpcId: "vpc-home" }, {}, "members in this VPC"],
    [{}, {}, "members, VPC not read"],
  ])(
    "places a peer group's members: pair %o, peer %o reads %s",
    (pair, peer, place) => {
      const peered = buildSecurityGroupMap(
        inputs(
          {
            VpcId: "vpc-home",
            IpPermissions: [
              {
                IpProtocol: "tcp",
                FromPort: 22,
                ToPort: 22,
                UserIdGroupPairs: [{ GroupId: "sg-peer", ...pair }],
              },
            ],
          },
          { peers: [{ GroupId: "sg-peer", GroupName: "peer", ...peer }] },
        ),
      )

      expect(
        peered.cards.find((c) => c.id === "in:peer:sg-peer")?.details,
      ).toStrictEqual(["sg-peer", place])
    },
  )

  it.each([
    ["0.0.0.0/0", undefined, ["anywhere"]],
    ["::/0", undefined, ["anywhere"]],
    ["10.0.0.0/8", undefined, []],
    ["0.0.0.0/0", "public web", ["public web"]],
  ])("notes %s with description %s as %o", (cidr, note, details) => {
    const one = buildSecurityGroupMap(
      inputs({
        IpPermissions: [
          {
            IpProtocol: "-1",
            ...(cidr.includes(":")
              ? { Ipv6Ranges: [{ CidrIpv6: cidr, Description: note }] }
              : { IpRanges: [{ CidrIp: cidr, Description: note }] }),
          },
        ],
      }),
    )

    expect(
      one.cards.find((c) => c.id === `in:cidr:${cidr}`)?.details,
    ).toStrictEqual(details)
  })

  it("draws one line per rule, into the group inbound and out of it outbound", () => {
    expect(
      map.links.map((l) => [l.source, l.target, l.label, l.protocol]),
    ).toStrictEqual([
      [
        `group:${GROUP_ID}`,
        "out:cidr:0.0.0.0/0",
        "outbound (all traffic)",
        "all",
      ],
      ["in:cidr:0.0.0.0/0", `group:${GROUP_ID}`, "inbound (tcp/443)", "tcp"],
      [
        "in:peer:sg-0000000000000app",
        `group:${GROUP_ID}`,
        "inbound (tcp/5432)",
        "tcp",
      ],
      [
        "in:prefix:pl-00000000000office",
        `group:${GROUP_ID}`,
        "inbound (tcp/22)",
        "tcp",
      ],
    ])
  })

  it("slots the rules a source shares, so their labels stack", () => {
    const shared = buildSecurityGroupMap(
      inputs({
        IpPermissions: [
          open({ IpProtocol: "tcp", FromPort: 80, ToPort: 80 }),
          open({ IpProtocol: "tcp", FromPort: 443, ToPort: 443 }),
          open({ IpProtocol: "icmp", FromPort: 8, ToPort: -1 }),
        ],
      }),
    )

    expect(shared.cards).toHaveLength(2)
    expect(shared.links.map((l) => [l.slot, l.slots])).toStrictEqual([
      [0, 3],
      [1, 3],
      [2, 3],
    ])
    expect(new Set(shared.links.map((l) => l.id)).size).toBe(3)
  })

  it("holds the budget for a group with many sources and members, collapsing the rest", () => {
    const sources = Array.from({ length: 300 }, (_, i) => ({
      CidrIp: `10.${Math.floor(i / 250)}.${i % 250}.0/24`,
    }))

    const big = buildSecurityGroupMap(
      inputs(
        {
          IpPermissions: [
            { IpProtocol: "tcp", FromPort: 22, ToPort: 22, IpRanges: sources },
            { IpProtocol: "tcp", FromPort: 80, ToPort: 80, IpRanges: sources },
          ],
          IpPermissionsEgress: [open({ IpProtocol: "-1" })],
        },
        {
          instances: Array.from({ length: 200 }, (_, i) => ({
            InstanceId: `i-${String(i).padStart(4, "0")}`,
            SecurityGroups: [{ GroupId: GROUP_ID }],
          })),
        },
      ),
    )

    const ids = new Set(big.cards.map((c) => c.id))

    expect(big.cards.length).toBeLessThanOrEqual(GROUP_MAP_BUDGET.cards)
    expect(big.links.length).toBeLessThanOrEqual(GROUP_MAP_BUDGET.links)
    expect(ids.has(`group:${GROUP_ID}`)).toBeTruthy()
    expect(ids.has("more:inbound")).toBeTruthy()
    expect(ids.has("more:group")).toBeTruthy()
    // Every candidate but the group, less those placed, less the two "more" cards.
    expect(ids.has("out:cidr:0.0.0.0/0")).toBeTruthy()
    expect(big.omitted).toBe(300 + 200 + 1 - (big.cards.length - 1 - 2))
    expect(
      big.links.every((l) => ids.has(l.source) && ids.has(l.target)),
    ).toBeTruthy()
  })
})

describe("layoutSecurityGroupMap", () => {
  const map = buildSecurityGroupMap(
    inputs(
      {
        IpPermissions: [
          open({ IpProtocol: "tcp", FromPort: 80, ToPort: 80 }),
          open({ IpProtocol: "tcp", FromPort: 443, ToPort: 443 }),
        ],
        IpPermissionsEgress: [open({ IpProtocol: "-1" })],
      },
      {
        instances: [
          { InstanceId: "i-a", SecurityGroups: [{ GroupId: GROUP_ID }] },
          { InstanceId: "i-b", SecurityGroups: [{ GroupId: GROUP_ID }] },
          { InstanceId: "i-c", SecurityGroups: [{ GroupId: GROUP_ID }] },
        ],
      },
    ),
  )

  it("puts sources above the group and destinations below it going down", () => {
    const { places } = layoutSecurityGroupMap(map, "down")
    const y = (id: string) => places.get(id)?.y ?? Number.NaN

    expect(y("in:cidr:0.0.0.0/0")).toBeLessThan(y(`group:${GROUP_ID}`))
    expect(y(`group:${GROUP_ID}`)).toBeLessThan(y("out:cidr:0.0.0.0/0"))
  })

  it("puts sources left of the group and destinations right of it going across", () => {
    const { places } = layoutSecurityGroupMap(map, "across")
    const x = (id: string) => places.get(id)?.x ?? Number.NaN

    expect(x("in:cidr:0.0.0.0/0")).toBeLessThan(x(`group:${GROUP_ID}`))
    expect(x(`group:${GROUP_ID}`)).toBeLessThan(x("out:cidr:0.0.0.0/0"))
  })

  it.each(["down", "across"] as const)(
    "boxes the members clear of the group and the destinations (%s)",
    (direction) => {
      const { places, members } = layoutSecurityGroupMap(map, direction)
      const box = members ?? { x: 0, y: 0, width: 0, height: 0 }
      const out = places.get("out:cidr:0.0.0.0/0") ?? { x: 0, y: 0 }

      for (const id of ["instance:i-a", "instance:i-b", "instance:i-c"]) {
        const at = places.get(id) ?? { x: Number.NaN, y: Number.NaN }

        expect(at.x).toBeGreaterThan(box.x)
        expect(at.y).toBeGreaterThan(box.y)
      }

      // The destinations sit past the box along the map's own axis.
      const past =
        direction === "down"
          ? out.y - (box.y + box.height)
          : out.x - (box.x + box.width)

      expect(past).toBeGreaterThan(0)
    },
  )

  it("gives each source room for its own stack of labels going across", () => {
    const ports = Array.from({ length: 12 }, (_, i) =>
      open({ IpProtocol: "tcp", FromPort: 8000 + i, ToPort: 8000 + i }),
    )

    const crowded = buildSecurityGroupMap(
      inputs({
        IpPermissions: [
          ...ports,
          {
            IpProtocol: "tcp",
            FromPort: 22,
            ToPort: 22,
            IpRanges: [{ CidrIp: "10.0.0.0/8" }, { CidrIp: "192.168.0.0/16" }],
          },
        ],
      }),
    )

    const { places } = layoutSecurityGroupMap(crowded, "across")

    // A card's centre, and the half-height of its card or its label stack.
    const reach = (id: string, rules: number) => ({
      centre: (places.get(id)?.y ?? Number.NaN) + 40,
      half: Math.max(40, rules * 10),
    })

    const sources = [
      reach("in:cidr:0.0.0.0/0", 12),
      reach("in:cidr:10.0.0.0/8", 1),
      reach("in:cidr:192.168.0.0/16", 1),
    ].toSorted((a, b) => a.centre - b.centre)

    for (const [i, next] of sources.slice(1).entries()) {
      const prev = sources[i] ?? next

      expect(next.centre - next.half).toBeGreaterThan(prev.centre + prev.half)
    }
  })

  it("leaves more room between the tiers for a taller stack of labels", () => {
    const one = buildSecurityGroupMap(
      inputs({ IpPermissions: [open({ IpProtocol: "-1" })] }),
    )

    const gap = (m: typeof map) => {
      const { places } = layoutSecurityGroupMap(m, "down")

      return (
        (places.get(`group:${GROUP_ID}`)?.y ?? 0) -
        (places.get("in:cidr:0.0.0.0/0")?.y ?? 0)
      )
    }

    expect(gap(map)).toBeGreaterThan(gap(one))
  })

  it.each(["down", "across"] as const)(
    "names each row of rules ahead of its first card (%s)",
    (direction) => {
      const { places, captions } = layoutSecurityGroupMap(map, direction)

      const row = (prefix: string) =>
        map.cards
          .filter((c) => c.id.startsWith(prefix))
          .map((c) => places.get(c.id) ?? { x: Number.NaN, y: Number.NaN })

      for (const [side, prefix] of [
        ["inbound", "in:"],
        ["outbound", "out:"],
      ] as const) {
        const caption = captions.get(side) ?? { x: Number.NaN, y: Number.NaN }

        for (const card of row(prefix)) {
          // Left of the row going down; above it going across.
          expect(
            direction === "down" ? card.x - caption.x : card.y - caption.y,
          ).toBeGreaterThan(direction === "down" ? 120 : 40)
        }
      }
    },
  )

  it("heads the members' box with the rules among them, above every member", () => {
    const among = buildSecurityGroupMap(
      inputs(
        {
          IpPermissions: [
            { IpProtocol: "-1", UserIdGroupPairs: [{ GroupId: GROUP_ID }] },
          ],
          IpPermissionsEgress: [
            { IpProtocol: "-1", UserIdGroupPairs: [{ GroupId: GROUP_ID }] },
          ],
        },
        {
          instances: [
            { InstanceId: "i-a", SecurityGroups: [{ GroupId: GROUP_ID }] },
          ],
        },
      ),
    )

    const plain = layoutSecurityGroupMap(map, "down")
    const headed = layoutSecurityGroupMap(among, "down")

    const below = (l: typeof plain) =>
      (l.places.get("instance:i-a")?.y ?? 0) - (l.members?.y ?? 0)

    expect(among.among.map((r) => r.label)).toStrictEqual([
      "inbound (all traffic)",
      "outbound (all traffic)",
    ])
    // A header line and one line per rule, 16px each.
    expect(below(headed) - below(plain)).toBe(48)
  })

  it("widens the gap going across for a long rule label", () => {
    const long = buildSecurityGroupMap(
      inputs({
        IpPermissions: [open({ IpProtocol: "icmp", FromPort: 3, ToPort: 4 })],
      }),
    )

    const gap = (m: typeof map) => {
      const { places } = layoutSecurityGroupMap(m, "across")

      return (
        (places.get(`group:${GROUP_ID}`)?.x ?? 0) -
        (places.get("in:cidr:0.0.0.0/0")?.x ?? 0)
      )
    }

    expect(gap(long)).toBeGreaterThan(gap(map))
  })
})
