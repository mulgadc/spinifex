import type {
  Instance,
  IpPermission,
  SecurityGroup,
  Tag,
} from "@aws-sdk/client-ec2"
import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import type { DBInstance } from "@aws-sdk/client-rds"

import type { LayoutDirection } from "@/lib/node-topology"

// Inbound rules' sources, the group with its members, then outbound rules'
// destinations.
export type GroupSide = "inbound" | "group" | "outbound"

export type GroupCardKind =
  | "group"
  | "self"
  | "peer"
  | "cidr"
  | "prefix"
  | "instance"
  | "lb"
  | "rds"
  | "more"

export type GroupResource = "instances" | "loadBalancers" | "dbInstances"

export type RuleProtocol = "tcp" | "udp" | "icmp" | "all" | "other"

export interface GroupCard {
  id: string
  kind: GroupCardKind
  side: GroupSide
  label: string
  details: string[]
  // The resource's own id, which its panel opens on.
  resourceId?: string
  // A member sits in the box beside the group, joined by no line.
  member?: boolean
  highlighted: boolean
}

export interface GroupLink {
  id: string
  source: string
  target: string
  label: string
  protocol: RuleProtocol
  // This line's place among the lines sharing its outer card, which its
  // label stacks by.
  slot: number
  slots: number
}

export interface GroupMap {
  cards: GroupCard[]
  links: GroupLink[]
  // Rule ends and members collapsed into a side's "+N more" card.
  omitted: number
  incomplete: GroupResource[]
  failed: GroupResource[]
}

export interface GroupInputs {
  group: SecurityGroup
  // Groups a rule names, read for their names.
  peers: SecurityGroup[]
  instances: Instance[]
  loadBalancers: LoadBalancer[]
  dbInstances: DBInstance[]
  incomplete: GroupResource[]
  failed: GroupResource[]
}

// The same hard limits as the network map, so a group's size never sets
// what the console renders.
export const GROUP_MAP_BUDGET = { cards: 120, links: 240 }

const PROTOCOL_NUMBERS = new Map([
  ["6", "tcp"],
  ["17", "udp"],
  ["1", "icmp"],
  ["58", "icmpv6"],
])

const ICMP_TYPES = new Map([
  [
    "icmp",
    new Map([
      [0, "echo-reply"],
      [3, "destination-unreachable"],
      [5, "redirect"],
      [8, "echo-request"],
      [11, "time-exceeded"],
    ]),
  ],
  [
    "icmpv6",
    new Map([
      [1, "destination-unreachable"],
      [2, "packet-too-big"],
      [3, "time-exceeded"],
      [128, "echo-request"],
      [129, "echo-reply"],
    ]),
  ],
])

// A rule's protocol by name: "-1" is every protocol, and the numbers AWS
// accepts for TCP, UDP, ICMP and ICMPv6 read as those names.
export function protocolName(rule: IpPermission): string {
  const raw = (rule.IpProtocol ?? "-1").toLowerCase()

  if (raw === "-1" || raw === "all") {
    return "all"
  }

  return PROTOCOL_NUMBERS.get(raw) ?? raw
}

export function protocolOf(rule: IpPermission): RuleProtocol {
  const name = protocolName(rule)

  if (name === "tcp" || name === "udp" || name === "all") {
    return name
  }

  return name === "icmp" || name === "icmpv6" ? "icmp" : "other"
}

function portRange(name: string, from?: number, to?: number): string {
  if (from === undefined || from === -1 || (from === 0 && to === 65_535)) {
    return `${name} all`
  }

  return from === to || to === undefined
    ? `${name} ${from}`
    : `${name} ${from}–${to}`
}

// For ICMP and ICMPv6, FromPort and ToPort are the type and the code.
function icmpLabel(name: string, type?: number, code?: number): string {
  if (type === undefined || type === -1) {
    return `${name} all`
  }

  const typeName = ICMP_TYPES.get(name)?.get(type)
  const numbers = `${type}/${code ?? -1}`

  return typeName ? `${name} ${typeName} (${numbers})` : `${name} ${numbers}`
}

// "tcp 22", "udp 1000–2000", "icmp echo-request (8/-1)", "all traffic".
export function ruleLabel(rule: IpPermission): string {
  const name = protocolName(rule)

  if (name === "all") {
    return "all traffic"
  }

  if (name === "tcp" || name === "udp") {
    return portRange(name, rule.FromPort, rule.ToPort)
  }

  if (name === "icmp" || name === "icmpv6") {
    return icmpLabel(name, rule.FromPort, rule.ToPort)
  }

  return `protocol ${name}`
}

function nameTag(tags: Tag[] | undefined): string | undefined {
  return tags?.find((t) => t.Key === "Name")?.Value
}

// One end of a rule, before it is placed on a side.
interface RuleEnd {
  key: string
  kind: GroupCardKind
  label: string
  details: string[]
  resourceId?: string
}

function ruleEnds(
  rule: IpPermission,
  groupId: string,
  peers: Map<string, SecurityGroup>,
): RuleEnd[] {
  const groups = (rule.UserIdGroupPairs ?? []).flatMap((pair): RuleEnd[] => {
    const id = pair.GroupId

    if (!id) {
      return []
    }

    // A group that names itself admits its own members.
    if (id === groupId) {
      return [
        {
          key: "self",
          kind: "self",
          label: "This group",
          details: ["its members, by private address"],
        },
      ]
    }

    const peer = peers.get(id)

    return [
      {
        key: `peer:${id}`,
        kind: "peer",
        label: peer?.GroupName ?? pair.GroupName ?? id,
        details: [id, "members, by private address"],
        resourceId: id,
      },
    ]
  })

  const cidrs = [
    ...(rule.IpRanges ?? []).map((r) => ({
      cidr: r.CidrIp,
      note: r.Description,
    })),
    ...(rule.Ipv6Ranges ?? []).map((r) => ({
      cidr: r.CidrIpv6,
      note: r.Description,
    })),
  ].flatMap(({ cidr, note }): RuleEnd[] =>
    cidr
      ? [
          {
            key: `cidr:${cidr}`,
            kind: "cidr",
            label: cidr,
            details: note ? [note] : [],
          },
        ]
      : [],
  )

  const prefixes = (rule.PrefixListIds ?? []).flatMap((p): RuleEnd[] =>
    p.PrefixListId
      ? [
          {
            key: `prefix:${p.PrefixListId}`,
            kind: "prefix",
            label: p.PrefixListId,
            details: p.Description ? [p.Description] : ["prefix list"],
          },
        ]
      : [],
  )

  return [...cidrs, ...groups, ...prefixes]
}

interface Candidate {
  card: GroupCard
  // The rules joining it to the group, each one line.
  rules: { label: string; protocol: RuleProtocol }[]
}

function sideCandidates(
  side: "inbound" | "outbound",
  rules: IpPermission[] | undefined,
  groupId: string,
  peers: Map<string, SecurityGroup>,
): Candidate[] {
  const byKey = new Map<string, Candidate>()

  for (const rule of rules ?? []) {
    const label = ruleLabel(rule)
    const protocol = protocolOf(rule)

    for (const end of ruleEnds(rule, groupId, peers)) {
      const id = `${side === "inbound" ? "in" : "out"}:${end.key}`
      const seen = byKey.get(id)

      if (seen) {
        seen.rules.push({ label, protocol })
        continue
      }

      byKey.set(id, {
        card: {
          id,
          kind: end.kind,
          side,
          label: end.label,
          details: end.details,
          resourceId: end.resourceId,
          highlighted: false,
        },
        rules: [{ label, protocol }],
      })
    }
  }

  return [...byKey.values()].toSorted((a, b) =>
    a.card.label.localeCompare(b.card.label),
  )
}

function memberCandidates(inputs: GroupInputs, groupId: string): Candidate[] {
  const holds = (ids: (string | undefined)[]) => ids.includes(groupId)

  const instances = inputs.instances
    .filter((i) => holds((i.SecurityGroups ?? []).map((g) => g.GroupId)))
    .map((i): GroupCard => {
      const id = i.InstanceId ?? ""
      const name = nameTag(i.Tags)

      return {
        id: `instance:${id}`,
        kind: "instance",
        side: "group",
        label: name ?? id,
        details: [...(name ? [id] : []), i.PrivateIpAddress ?? ""].filter(
          Boolean,
        ),
        resourceId: id,
        member: true,
        highlighted: false,
      }
    })

  const lbs = inputs.loadBalancers
    .filter((lb) => holds(lb.SecurityGroups ?? []))
    .map((lb): GroupCard => ({
      id: `lb:${lb.LoadBalancerArn ?? lb.LoadBalancerName ?? ""}`,
      kind: "lb",
      side: "group",
      label: lb.LoadBalancerName ?? "",
      details: [lb.Type ?? "", lb.Scheme ?? ""].filter(Boolean),
      resourceId: lb.LoadBalancerArn,
      member: true,
      highlighted: false,
    }))

  const dbs = inputs.dbInstances
    .filter((db) =>
      holds((db.VpcSecurityGroups ?? []).map((g) => g.VpcSecurityGroupId)),
    )
    .map((db): GroupCard => ({
      id: `rds:${db.DBInstanceIdentifier ?? ""}`,
      kind: "rds",
      side: "group",
      label: db.DBInstanceIdentifier ?? "",
      details: [db.Engine ?? ""].filter(Boolean),
      resourceId: db.DBInstanceIdentifier,
      member: true,
      highlighted: false,
    }))

  return [...instances, ...lbs, ...dbs]
    .toSorted(byName)
    .map((card) => ({ card, rules: [] }))
}

function byName(a: GroupCard, b: GroupCard) {
  return a.label.localeCompare(b.label)
}

const MORE_LABELS: Record<GroupSide, string> = {
  inbound: "sources",
  group: "members",
  outbound: "destinations",
}

// Lines for one placed card's rules, each slotted for its stacked label.
function linesFor(candidate: Candidate, groupCard: string): GroupLink[] {
  const { card, rules } = candidate
  const inbound = card.side === "inbound"

  return rules.map((rule, slot) => ({
    id: `${card.id}>${rule.label}:${slot}`,
    source: inbound ? card.id : groupCard,
    target: inbound ? groupCard : card.id,
    label: rule.label,
    protocol: rule.protocol,
    slot,
    slots: rules.length,
  }))
}

// A read-only map of one security group: who its inbound rules admit, the
// resources it covers, and where its outbound rules allow traffic. Over budget,
// each side keeps what fits, in name order, and collapses the rest.
export function buildSecurityGroupMap(inputs: GroupInputs): GroupMap {
  const groupId = inputs.group.GroupId ?? ""
  const groupCard = `group:${groupId}`
  const peers = new Map(inputs.peers.map((p) => [p.GroupId ?? "", p]))

  const cards: GroupCard[] = [
    {
      id: groupCard,
      kind: "group",
      side: "group",
      label: inputs.group.GroupName ?? groupId,
      details: [groupId, inputs.group.Description ?? ""].filter(Boolean),
      resourceId: groupId,
      highlighted: true,
    },
  ]

  const links: GroupLink[] = []

  const rules: [GroupSide, Candidate[]][] = [
    [
      "inbound",
      sideCandidates("inbound", inputs.group.IpPermissions, groupId, peers),
    ],
    [
      "outbound",
      sideCandidates(
        "outbound",
        inputs.group.IpPermissionsEgress,
        groupId,
        peers,
      ),
    ],
  ]

  // The shorter side of rules first, so a crowded side does not hide the
  // other whole; members, which have no lines, last.
  const sides: [GroupSide, Candidate[]][] = [
    ...rules.toSorted((a, b) => a[1].length - b[1].length),
    ["group", memberCandidates(inputs, groupId)],
  ]

  // One card and one line per side are kept back for its "+N more".
  const cardRoom = GROUP_MAP_BUDGET.cards - 1 - sides.length
  const linkRoom = GROUP_MAP_BUDGET.links - sides.length
  let full = false
  let omitted = 0

  for (const [side, candidates] of sides) {
    const left: Candidate[] = []

    for (const candidate of candidates) {
      const lines = linesFor(candidate, groupCard)

      full ||=
        cards.length >= cardRoom + 1 || links.length + lines.length > linkRoom

      if (full) {
        left.push(candidate)
        continue
      }

      cards.push(candidate.card)
      links.push(...lines)
    }

    if (left.length === 0) {
      continue
    }

    omitted += left.length

    const more: GroupCard = {
      id: `more:${side}`,
      kind: "more",
      side,
      label: `+${left.length} more ${MORE_LABELS[side]}`,
      details: [],
      member: side === "group",
      highlighted: false,
    }

    cards.push(more)

    if (side !== "group") {
      const inbound = side === "inbound"

      links.push({
        id: `${more.id}>${groupCard}`,
        source: inbound ? more.id : groupCard,
        target: inbound ? groupCard : more.id,
        label: "more rules",
        protocol: "other",
        slot: 0,
        slots: 1,
      })
    }
  }

  return {
    cards,
    links,
    omitted,
    incomplete: inputs.incomplete,
    failed: inputs.failed,
  }
}

export interface GroupPlace {
  x: number
  y: number
}

export interface GroupBox {
  x: number
  y: number
  width: number
  height: number
}

export interface GroupLayout {
  places: Map<string, GroupPlace>
  // The dashed box holding the group's members, when it has any.
  members?: GroupBox
}

// Sizes are px at a 16px root, as the network map's are.
const CARD_WIDTH = 220
const CARD_HEIGHT = 80
const GAP = 32
const BOX_PAD = 12
const BOX_HEAD = 24
const BOX_COLUMNS = 2
const MEMBER_HEIGHT = 72

// A rule's label stacks by its outer card, one row per rule.
const LABEL_ROW = 20
const LABEL_BASE = 72

// Across the map, a side's cards sit side by side going down and stacked
// going across; between sides sits room for the tallest stack of labels.
export function layoutSecurityGroupMap(
  map: GroupMap,
  direction: LayoutDirection,
): GroupLayout {
  const places = new Map<string, GroupPlace>()
  const down = direction === "down"

  const at = (along: number, deep: number): GroupPlace =>
    down ? { x: along, y: deep } : { x: deep, y: along }

  const stack = Math.max(1, ...map.links.map((l) => l.slots))
  // Going across, labels stack alongside the line, so the gap need only fit one wide label.
  const gap = down ? LABEL_BASE + stack * LABEL_ROW : 2 * LABEL_BASE + 64
  const depthOf = down ? CARD_HEIGHT : CARD_WIDTH

  // Going across, a card's rule labels stack centred on it, so each card
  // takes the taller of itself and its stack.
  const slotsOf = new Map<string, number>()

  for (const link of map.links) {
    for (const end of [link.source, link.target]) {
      slotsOf.set(end, Math.max(slotsOf.get(end) ?? 0, link.slots))
    }
  }

  const span = (id: string) =>
    down
      ? CARD_WIDTH
      : Math.max(CARD_HEIGHT, (slotsOf.get(id) ?? 0) * LABEL_ROW)

  const row = (side: "inbound" | "outbound", deep: number) => {
    const ids = map.cards.filter((c) => c.side === side).map((c) => c.id)
    const total = ids.reduce((sum, id) => sum + span(id), 0)
    let along = -(total + (ids.length - 1) * GAP) / 2

    for (const id of ids) {
      const size = span(id)
      const card = down ? CARD_WIDTH : CARD_HEIGHT

      places.set(id, at(along + (size - card) / 2, deep))
      along += size + GAP
    }
  }

  const group = map.cards.find((c) => c.kind === "group")
  const members = map.cards.filter((c) => c.member)

  row("inbound", 0)

  const groupDeep = depthOf + gap

  if (group) {
    places.set(group.id, at(-(down ? CARD_WIDTH : CARD_HEIGHT) / 2, groupDeep))
  }

  let middleDepth = depthOf
  let box: GroupBox | undefined

  if (members.length > 0) {
    const columns = Math.min(members.length, BOX_COLUMNS)
    const rows = Math.ceil(members.length / columns)
    const width = 2 * BOX_PAD + columns * CARD_WIDTH + (columns - 1) * BOX_PAD
    const height = BOX_HEAD + rows * (MEMBER_HEIGHT + BOX_PAD)

    // Beside the group going down, beneath it going across.
    const corner = down
      ? { x: CARD_WIDTH / 2 + GAP, y: groupDeep }
      : { x: groupDeep, y: CARD_HEIGHT / 2 + GAP }

    box = { ...corner, width, height }
    middleDepth = Math.max(middleDepth, down ? height : width)

    for (const [i, card] of members.entries()) {
      places.set(card.id, {
        x: corner.x + BOX_PAD + (i % columns) * (CARD_WIDTH + BOX_PAD),
        y:
          corner.y +
          BOX_HEAD +
          Math.floor(i / columns) * (MEMBER_HEIGHT + BOX_PAD),
      })
    }
  }

  row("outbound", groupDeep + middleDepth + gap)

  return { places, members: box }
}

export function scaleGroupLayout(
  layout: GroupLayout,
  unit: number,
): GroupLayout {
  const scale = <T extends GroupPlace>(p: T): T => ({
    ...p,
    x: p.x * unit,
    y: p.y * unit,
  })

  return {
    places: new Map([...layout.places].map(([id, p]) => [id, scale(p)])),
    members: layout.members && {
      ...scale(layout.members),
      width: layout.members.width * unit,
      height: layout.members.height * unit,
    },
  }
}
