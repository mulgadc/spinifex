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
    [{ IpProtocol: "tcp", FromPort: 22, ToPort: 22 }, "tcp 22"],
    [{ IpProtocol: "udp", FromPort: 1000, ToPort: 2000 }, "udp 1000–2000"],
    [{ IpProtocol: "tcp", FromPort: 0, ToPort: 65_535 }, "tcp all"],
    [{ IpProtocol: "6", FromPort: 443, ToPort: 443 }, "tcp 443"],
    [{ IpProtocol: "17", FromPort: 53, ToPort: 53 }, "udp 53"],
    [{ IpProtocol: "-1" }, "all traffic"],
    [{ IpProtocol: "47" }, "protocol 47"],
  ])("labels %o as %s", (rule, label) => {
    expect(ruleLabel(rule)).toBe(label)
  })

  it.each([
    [
      { IpProtocol: "icmp", FromPort: 8, ToPort: -1 },
      "icmp echo-request (8/-1)",
    ],
    [
      { IpProtocol: "1", FromPort: 3, ToPort: 4 },
      "icmp destination-unreachable (3/4)",
    ],
    [{ IpProtocol: "icmp", FromPort: -1, ToPort: -1 }, "icmp all"],
    [{ IpProtocol: "icmp", FromPort: 13, ToPort: 0 }, "icmp 13/0"],
    [
      { IpProtocol: "58", FromPort: 128, ToPort: -1 },
      "icmpv6 echo-request (128/-1)",
    ],
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
      peers: [{ GroupId: "sg-0000000000000app", GroupName: "app" }],
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
      ["in:self", "inbound"],
      ["lb:front", "group"],
      ["instance:i-in", "group"],
      ["rds:orders", "group"],
    ])
    expect(card(`group:${GROUP_ID}`)?.highlighted).toBeTruthy()
  })

  it("names a group a rule admits, and says it matches private addresses", () => {
    expect(card("in:peer:sg-0000000000000app")).toMatchObject({
      label: "app",
      details: ["sg-0000000000000app", "members, by private address"],
      resourceId: "sg-0000000000000app",
    })
    expect(card("in:self")?.label).toBe("This group")
  })

  it("draws one line per rule, into the group inbound and out of it outbound", () => {
    expect(
      map.links.map((l) => [l.source, l.target, l.label, l.protocol]),
    ).toStrictEqual([
      [`group:${GROUP_ID}`, "out:cidr:0.0.0.0/0", "all traffic", "all"],
      ["in:cidr:0.0.0.0/0", `group:${GROUP_ID}`, "tcp 443", "tcp"],
      ["in:peer:sg-0000000000000app", `group:${GROUP_ID}`, "tcp 5432", "tcp"],
      ["in:prefix:pl-00000000000office", `group:${GROUP_ID}`, "tcp 22", "tcp"],
      ["in:self", `group:${GROUP_ID}`, "all traffic", "all"],
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
})
