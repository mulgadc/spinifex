import type {
  Address,
  Instance,
  InternetGateway,
  NatGateway,
  RouteTable,
  Subnet,
  Tag,
  Vpc,
} from "@aws-sdk/client-ec2"
import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import type { DBInstance } from "@aws-sdk/client-rds"

import type { LayoutDirection } from "@/lib/node-topology"
import type { Reachability } from "@/lib/reachability"
import { routeTableFor, subnetType } from "@/lib/subnet-routing"

// The map never draws more than this, whatever the account holds.
export const MAP_BUDGET = { cards: 120, edges: 240 }

export const MAP_TIERS = [
  "internet",
  "gateways",
  "vpc",
  "subnets",
  "resources",
] as const

export type MapTier = (typeof MAP_TIERS)[number]

export type MapKind =
  | "internet"
  | "igw"
  | "nat"
  | "eip"
  | "vpc"
  | "rds"
  | "subnet"
  | "instance"
  | "lb"
  | "more"

// The resource types the map reads, each read on its own.
export type MapResource =
  | "subnets"
  | "routeTables"
  | "internetGateways"
  | "natGateways"
  | "addresses"
  | "instances"
  | "loadBalancers"
  | "dbInstances"

// A labelled value on a card, as a node card lists its ports.
export interface MapRow {
  label: string
  value: string
}

export interface MapCard {
  id: string
  kind: MapKind
  tier: MapTier
  label: string
  details: string[]
  rows?: MapRow[]
  // The Internet card's state.
  reach?: Reachability
  // The resource's own id, which its panel opens on.
  resourceId?: string
  // The subnet a resource stacks under, or a subnet's availability zone.
  column?: string
  highlighted: boolean
  dimmed: boolean
  // A "+N more" card: how many of each kind it stands for.
  stands?: { kind: MapKind; count: number }[]
}

export interface MapLink {
  id: string
  // Always the card nearer the internet, so every line runs down the map.
  source: string
  target: string
  label?: string
  heavy?: boolean
}

export interface NetworkMap {
  cards: MapCard[]
  links: MapLink[]
  // Resources left out to keep the map inside its budget.
  omitted: number
  // Types that stopped at the page limit, and types whose read failed.
  incomplete: MapResource[]
  failed: MapResource[]
}

export interface NetworkInputs {
  vpc: Vpc
  subnets: Subnet[]
  routeTables: RouteTable[]
  internetGateways: InternetGateway[]
  natGateways: NatGateway[]
  addresses: Address[]
  instances: Instance[]
  loadBalancers: LoadBalancer[]
  dbInstances: DBInstance[]
  incomplete: MapResource[]
  failed: MapResource[]
}

// The resource whose panel draws the map; all but the VPC dim the rest.
export interface MapFocus {
  kind: "vpc" | "subnet" | "routeTable" | "igw" | "nat" | "instance"
  id: string
}

type Draft = Omit<MapCard, "highlighted" | "dimmed">

function nameOf(tags: Tag[] | undefined): string | undefined {
  return tags?.find((tag) => tag.Key === "Name")?.Value
}

function labelled(id: string, tags: Tag[] | undefined) {
  const name = nameOf(tags)

  return { label: name ?? id, details: name ? [id] : [] }
}

function compact(values: (string | undefined)[]): string[] {
  return values.filter((value): value is string => Boolean(value))
}

// Gateways and route targets come first, then subnets, then what runs in them.
const KIND_RANK: Record<MapKind, number> = {
  internet: 0,
  vpc: 0,
  igw: 1,
  nat: 1,
  eip: 1,
  subnet: 2,
  instance: 3,
  lb: 3,
  rds: 3,
  more: 4,
}

function byRankThenName(a: Draft, b: Draft): number {
  return KIND_RANK[a.kind] - KIND_RANK[b.kind] || a.label.localeCompare(b.label)
}

const LIVE_NAT = new Set(["pending", "available"])
const GONE_INSTANCE = new Set(["terminated", "shutting-down"])

// The cards and lines drawn before the budget decides which are kept.
interface Sketch {
  vpcId: string
  cards: Draft[]
  links: MapLink[]
  // Every route table was read, so a subnet's type can be decided.
  routesKnown: boolean
}

function link(
  sketch: Sketch,
  source: string,
  target: string,
  extra: Pick<MapLink, "label" | "heavy"> = {},
) {
  const suffix = extra.label ? `:${extra.label}` : ""

  sketch.links.push({
    id: `${source}>${target}${suffix}`,
    source,
    target,
    ...extra,
  })
}

function sketchFrame(sketch: Sketch, vpc: Vpc) {
  const { vpcId } = sketch
  const name = nameOf(vpc.Tags)

  sketch.cards.push(
    {
      id: "internet",
      kind: "internet",
      tier: "internet",
      label: "Internet",
      details: [],
    },
    {
      id: `vpc:${vpcId}`,
      kind: "vpc",
      tier: "vpc",
      resourceId: vpcId,
      label: name ?? vpcId,
      details: compact([name ? vpcId : undefined, vpc.CidrBlock]),
    },
  )
}

function sketchInternetGateways(sketch: Sketch, igws: InternetGateway[]) {
  const attached = igws.filter((igw) =>
    igw.Attachments?.some((a) => a.VpcId === sketch.vpcId),
  )

  for (const igw of attached) {
    const id = igw.InternetGatewayId ?? ""

    sketch.cards.push({
      id: `igw:${id}`,
      kind: "igw",
      tier: "gateways",
      resourceId: id,
      ...labelled(id, igw.Tags),
    })
    link(sketch, "internet", `igw:${id}`, { heavy: true })
    link(sketch, `igw:${id}`, `vpc:${sketch.vpcId}`)
  }
}

// A NAT gateway reaches the internet only through its own subnet's route to
// an internet gateway, and only outbound.
function sketchNatEgress(
  sketch: Sketch,
  nat: NatGateway,
  subnets: Subnet[],
  routeTables: RouteTable[],
) {
  const home = subnets.find((s) => s.SubnetId === nat.SubnetId)
  const table = home && routeTableFor(home, routeTables, sketch.routesKnown)

  const igws = new Set(
    compact((table?.Routes ?? []).map((r) => r.GatewayId)).filter((g) =>
      g.startsWith("igw-"),
    ),
  )

  for (const igw of igws) {
    link(sketch, `igw:${igw}`, `nat:${nat.NatGatewayId ?? ""}`, {
      label: "outbound only",
      heavy: true,
    })
  }
}

function sketchNatGateways(
  sketch: Sketch,
  nats: NatGateway[],
  subnets: Subnet[],
  routeTables: RouteTable[],
) {
  for (const nat of nats) {
    const id = nat.NatGatewayId ?? ""
    const named = labelled(id, nat.Tags)

    sketch.cards.push({
      id: `nat:${id}`,
      kind: "nat",
      tier: "gateways",
      resourceId: id,
      label: named.label,
      details: [...named.details, nat.ConnectivityType ?? "public"],
    })

    if (nat.SubnetId) {
      link(sketch, `nat:${id}`, `subnet:${nat.SubnetId}`)
    }

    if (nat.ConnectivityType !== "private") {
      sketchNatEgress(sketch, nat, subnets, routeTables)
    }
  }
}

// Each route to a gateway is its own line, labelled with its destination.
function sketchRoutes(sketch: Sketch, subnetCard: string, table: RouteTable) {
  for (const route of table.Routes ?? []) {
    const label = route.DestinationCidrBlock ?? route.DestinationIpv6CidrBlock

    if (route.GatewayId?.startsWith("igw-")) {
      link(sketch, `igw:${route.GatewayId}`, subnetCard, { label })
    } else if (route.NatGatewayId) {
      link(sketch, `nat:${route.NatGatewayId}`, subnetCard, { label })
    }
  }
}

function sketchSubnets(
  sketch: Sketch,
  subnets: Subnet[],
  routeTables: RouteTable[],
) {
  for (const subnet of subnets) {
    const id = subnet.SubnetId ?? ""
    const named = labelled(id, subnet.Tags)
    const table = routeTableFor(subnet, routeTables, sketch.routesKnown)

    sketch.cards.push({
      id: `subnet:${id}`,
      kind: "subnet",
      tier: "subnets",
      resourceId: id,
      column: subnet.AvailabilityZone,
      label: named.label,
      details: compact([
        ...named.details,
        `${subnet.CidrBlock ?? ""} · ${subnet.AvailabilityZone ?? ""}`,
        table?.RouteTableId,
      ]),
      rows: [
        {
          label: "Type",
          value: subnetType(subnet, routeTables, sketch.routesKnown),
        },
      ],
    })
    link(sketch, `vpc:${sketch.vpcId}`, `subnet:${id}`)

    if (table) {
      sketchRoutes(sketch, `subnet:${id}`, table)
    }
  }
}

function sketchInstances(sketch: Sketch, instances: Instance[]) {
  for (const instance of instances) {
    const id = instance.InstanceId ?? ""
    const named = labelled(id, instance.Tags)

    sketch.cards.push({
      id: `instance:${id}`,
      kind: "instance",
      tier: "resources",
      resourceId: id,
      column: instance.SubnetId,
      label: named.label,
      details: named.details,
      rows: [
        { label: "Public IP", value: instance.PublicIpAddress ?? "-" },
        { label: "Private IP", value: instance.PrivateIpAddress ?? "-" },
      ],
    })

    if (instance.SubnetId) {
      link(sketch, `subnet:${instance.SubnetId}`, `instance:${id}`)
    }
  }
}

function sketchLoadBalancers(sketch: Sketch, lbs: LoadBalancer[]) {
  for (const lb of lbs.filter((l) => l.VpcId === sketch.vpcId)) {
    const arn = lb.LoadBalancerArn ?? ""
    const homes = compact((lb.AvailabilityZones ?? []).map((z) => z.SubnetId))

    sketch.cards.push({
      id: `lb:${arn}`,
      kind: "lb",
      tier: "resources",
      resourceId: arn,
      column: homes[0],
      label: lb.LoadBalancerName ?? arn,
      details: compact([lb.Type, lb.Scheme]),
    })

    for (const home of homes) {
      link(sketch, `subnet:${home}`, `lb:${arn}`)
    }
  }
}

// The API names a database's subnet group, not the subnet it runs in.
function sketchDatabases(sketch: Sketch, dbs: DBInstance[]) {
  for (const db of dbs) {
    const group = db.DBSubnetGroup

    if (group?.VpcId !== sketch.vpcId) {
      continue
    }

    const id = db.DBInstanceIdentifier ?? ""

    sketch.cards.push({
      id: `rds:${id}`,
      kind: "rds",
      tier: "vpc",
      resourceId: id,
      label: id,
      details: compact([
        db.Engine,
        group.DBSubnetGroupName && `subnet group ${group.DBSubnetGroupName}`,
      ]),
    })
    link(sketch, `vpc:${sketch.vpcId}`, `rds:${id}`)
  }
}

// An Elastic IP is drawn only when it is held by a NAT gateway or an instance
// on this map, joined to its holder.
function sketchAddresses(
  sketch: Sketch,
  addresses: Address[],
  nats: NatGateway[],
  instances: Instance[],
) {
  const natByAllocation = new Map(
    nats.flatMap((nat) =>
      (nat.NatGatewayAddresses ?? []).map((a) => [a.AllocationId, nat]),
    ),
  )

  const instanceIds = new Set(instances.map((i) => i.InstanceId))

  const holderOf = (address: Address): string | undefined => {
    const nat = natByAllocation.get(address.AllocationId)

    if (nat) {
      return `nat:${nat.NatGatewayId}`
    }

    return address.InstanceId && instanceIds.has(address.InstanceId)
      ? `instance:${address.InstanceId}`
      : undefined
  }

  for (const address of addresses) {
    const holder = holderOf(address)

    if (!holder || !address.PublicIp) {
      continue
    }

    const id = `eip:${address.AllocationId ?? address.PublicIp}`

    sketch.cards.push({
      id,
      kind: "eip",
      tier: "gateways",
      resourceId: address.AllocationId,
      label: address.PublicIp,
      details: compact([address.AllocationId]),
    })
    link(sketch, id, holder)
  }
}

function sketchMap(inputs: NetworkInputs): Sketch {
  const vpcId = inputs.vpc.VpcId ?? ""
  const gaps = new Set([...inputs.incomplete, ...inputs.failed])
  const subnets = inputs.subnets.filter((subnet) => subnet.VpcId === vpcId)

  const sketch: Sketch = {
    vpcId,
    cards: [],
    links: [],
    routesKnown: !gaps.has("routeTables"),
  }

  const nats = inputs.natGateways.filter(
    (nat) => nat.VpcId === vpcId && LIVE_NAT.has(nat.State ?? ""),
  )

  const instances = inputs.instances.filter(
    (i) => i.VpcId === vpcId && !GONE_INSTANCE.has(i.State?.Name ?? ""),
  )

  sketchFrame(sketch, inputs.vpc)
  sketchInternetGateways(sketch, inputs.internetGateways)
  sketchNatGateways(sketch, nats, subnets, inputs.routeTables)
  sketchSubnets(sketch, subnets, inputs.routeTables)
  sketchInstances(sketch, instances)
  sketchLoadBalancers(sketch, inputs.loadBalancers)
  sketchDatabases(sketch, inputs.dbInstances)
  sketchAddresses(sketch, inputs.addresses, nats, instances)

  return sketch
}

// Reachable when an attached internet gateway is the target of some route.
// Short of that, a gap in either read leaves it unknown, not unreachable.
export function internetReach(inputs: NetworkInputs): Reachability {
  const vpcId = inputs.vpc.VpcId

  const attached = new Set(
    inputs.internetGateways
      .filter((igw) => igw.Attachments?.some((a) => a.VpcId === vpcId))
      .map((igw) => igw.InternetGatewayId),
  )

  const routed = inputs.routeTables.some(
    (rt) =>
      rt.VpcId === vpcId &&
      rt.Routes?.some((r) => r.GatewayId && attached.has(r.GatewayId)),
  )

  if (routed) {
    return "reachable"
  }

  const gaps = new Set([...inputs.incomplete, ...inputs.failed])

  return gaps.has("internetGateways") || gaps.has("routeTables")
    ? "unknown"
    : "unreachable"
}

// A route line carries its destination; a resource's line to its subnet does not.
const isRoute = (l: MapLink) => l.label !== undefined && !l.heavy

// The cards a panel highlights: what the resource is, and what it touches.
function focusSet(
  focus: MapFocus,
  sketch: Sketch,
  routeTables: RouteTable[],
): Set<string> {
  const self = `${focus.kind}:${focus.id}`

  const from = (id: string, keep: (l: MapLink) => boolean) =>
    sketch.links
      .filter((l) => keep(l) && (l.source === id || l.target === id))
      .map((l) => (l.source === id ? l.target : l.source))

  if (focus.kind === "subnet") {
    const mine = sketch.cards.filter(
      (c) => c.tier === "resources" && c.column === focus.id,
    )

    const nats = from(self, (l) => !isRoute(l)).filter((id) =>
      id.startsWith("nat:"),
    )

    return new Set([self, ...mine.map((c) => c.id), ...nats])
  }

  if (focus.kind === "igw") {
    return new Set([self, ...from(self, isRoute)])
  }

  if (focus.kind === "nat") {
    const near = from(self, (l) => !l.heavy).filter(
      (id) => !id.startsWith("vpc:"),
    )

    return new Set([self, ...near])
  }

  if (focus.kind === "instance") {
    const subnet = sketch.cards.find((c) => c.id === self)?.column

    const eips = from(self, () => true).filter((id) => id.startsWith("eip:"))

    return new Set([self, ...(subnet ? [`subnet:${subnet}`] : []), ...eips])
  }

  if (focus.kind === "routeTable") {
    return routeTableSet(focus.id, sketch, routeTables)
  }

  return new Set([self])
}

// A route table has no card: it lights the subnets it serves and its targets.
function routeTableSet(
  id: string,
  sketch: Sketch,
  routeTables: RouteTable[],
): Set<string> {
  const table = routeTables.find((rt) => rt.RouteTableId === id)

  if (!table) {
    return new Set()
  }

  const explicit = new Set(
    routeTables.flatMap((rt) =>
      compact((rt.Associations ?? []).map((a) => a.SubnetId)),
    ),
  )

  const main = table.Associations?.some((a) => a.Main) ?? false

  const mine = new Set(
    compact((table.Associations ?? []).map((a) => a.SubnetId)),
  )

  // Only a complete read says which subnets fall back to the main table.
  const served = sketch.cards.filter(
    (c) =>
      c.kind === "subnet" &&
      c.resourceId !== undefined &&
      (mine.has(c.resourceId) ||
        (main && sketch.routesKnown && !explicit.has(c.resourceId))),
  )

  const targets = (table.Routes ?? []).flatMap((r) => {
    if (r.GatewayId?.startsWith("igw-")) {
      return [`igw:${r.GatewayId}`]
    }

    return r.NatGatewayId ? [`nat:${r.NatGatewayId}`] : []
  })

  return new Set([...served.map((c) => c.id), ...targets])
}

// The order cards claim the budget: the selection, the map's frame, what the
// selection lights, its neighbours, then everything else, each kind by name.
function claimOrder(
  cards: Draft[],
  links: MapLink[],
  selected: string,
  focused: Set<string>,
) {
  const frame = cards
    .filter((c) => ["internet", "vpc", "igw", "nat"].includes(c.kind))
    .toSorted(byRankThenName)

  const touching = new Set(
    links.flatMap((l) => {
      if (focused.has(l.source)) {
        return [l.target]
      }

      return focused.has(l.target) ? [l.source] : []
    }),
  )

  const neighbours = cards
    .filter((c) => touching.has(c.id))
    .toSorted(byRankThenName)

  const rest = cards.toSorted(byRankThenName)

  // A set keeps each card's first, highest-priority place.
  return new Set(
    [
      ...cards.filter((c) => c.id === selected),
      ...frame,
      ...cards.filter((c) => focused.has(c.id)).toSorted(byRankThenName),
      ...neighbours,
      ...rest,
    ].map((c) => c.id),
  )
}

// What a tier's "+N more" card hangs from, which the frame always places.
function anchorOf(tier: MapTier, vpcCard: string): string {
  return tier === "gateways" ? "internet" : vpcCard
}

export function buildNetworkMap(
  inputs: NetworkInputs,
  focus: MapFocus,
  budget = MAP_BUDGET,
): NetworkMap {
  const sketch = sketchMap(inputs)
  const { cards, links } = sketch
  const vpcCard = `vpc:${inputs.vpc.VpcId ?? ""}`
  const highlight = focusSet(focus, sketch, inputs.routeTables)
  // One card and one line in each tier are kept back for its "+N more".
  const cardRoom = budget.cards - MAP_TIERS.length
  const edgeRoom = budget.edges - MAP_TIERS.length

  const ends = new Map<string, MapLink[]>()

  const endAt = (id: string, l: MapLink) => {
    const list = ends.get(id)

    if (list) {
      list.push(l)
    } else {
      ends.set(id, [l])
    }
  }

  for (const l of links) {
    endAt(l.source, l)

    if (l.target !== l.source) {
      endAt(l.target, l)
    }
  }

  const placed = new Set<string>()
  let lines = 0

  for (const id of claimOrder(
    cards,
    links,
    `${focus.kind}:${focus.id}`,
    highlight,
  )) {
    // The lines this card adds: those whose other end is already placed.
    const joining = (ends.get(id) ?? []).filter((l) =>
      placed.has(l.source === id ? l.target : l.source),
    ).length

    if (placed.size + 1 > cardRoom || lines + joining > edgeRoom) {
      break
    }

    placed.add(id)
    lines += joining
  }

  const kept = cards.filter((c) => placed.has(c.id))
  const left = cards.filter((c) => !placed.has(c.id))

  const keptLinks = links.filter(
    (l) => placed.has(l.source) && placed.has(l.target),
  )

  const more: Draft[] = []

  for (const tier of MAP_TIERS) {
    const out = left.filter((c) => c.tier === tier)

    if (out.length === 0) {
      continue
    }

    const kinds = [...new Set(out.map((c) => c.kind))]

    more.push({
      id: `more:${tier}`,
      kind: "more",
      tier,
      label: `+${out.length} more`,
      details: [],
      stands: kinds.map((kind) => ({
        kind,
        count: out.filter((c) => c.kind === kind).length,
      })),
    })
    keptLinks.push({
      id: `${anchorOf(tier, vpcCard)}>more:${tier}`,
      source: anchorOf(tier, vpcCard),
      target: `more:${tier}`,
    })
  }

  // The VPC is the whole map, so selecting it dims nothing.
  const dims = focus.kind !== "vpc"
  const reach = internetReach(inputs)

  return {
    cards: [...kept, ...more].map((card) => ({
      ...card,
      ...(card.kind === "internet" && { reach }),
      highlighted: highlight.has(card.id),
      dimmed: dims && !highlight.has(card.id),
    })),
    links: keptLinks,
    omitted: left.length,
    incomplete: inputs.incomplete,
    failed: inputs.failed,
  }
}

export interface MapPlace {
  x: number
  y: number
}

export type BoxKind = "instance" | "lb"

// A dashed container holding one subnet's resources of one kind.
export interface MapBox {
  id: string
  kind: BoxKind
  label: string
  x: number
  y: number
  width: number
  height: number
}

export interface MapLayout {
  places: Map<string, MapPlace>
  boxes: MapBox[]
}

// Sizes are px at a 16px root; scaleLayout fits them to the console's own.
export const CARD_WIDTH = 220
const GAP = 32
const CARD_HEIGHT = 80
const SUBNET_HEIGHT = 124
const BOX_PAD = 12
const BOX_HEAD = 24
const BOX_COLUMNS = 2
const LABEL_ROOM = 96

const ITEM_HEIGHT: Record<BoxKind, number> = { instance: 104, lb: 80 }

const BOX_LABELS: Record<BoxKind, string> = {
  instance: "Instances",
  lb: "Load balancers",
}

// How far each tier sits from the internet, down the map or across it.
const DEPTH: Record<LayoutDirection, Record<MapTier, number>> = {
  down: { internet: 0, gateways: 130, vpc: 270, subnets: 410, resources: 570 },
  across: {
    internet: 0,
    gateways: 300,
    vpc: 600,
    subnets: 900,
    resources: 1200,
  },
}

interface Grid {
  kind: BoxKind
  width: number
  height: number
  // Each card's offset from the box's corner.
  cells: [string, MapPlace][]
}

function grid(kind: BoxKind, cards: MapCard[]): Grid {
  const columns = Math.min(cards.length, BOX_COLUMNS)
  const rows = Math.ceil(cards.length / columns)
  const item = ITEM_HEIGHT[kind]

  return {
    kind,
    width: 2 * BOX_PAD + columns * CARD_WIDTH + (columns - 1) * BOX_PAD,
    height: BOX_HEAD + rows * item + rows * BOX_PAD,
    cells: cards.toSorted(byRankThenName).map((card, i) => [
      card.id,
      {
        x: BOX_PAD + (i % columns) * (CARD_WIDTH + BOX_PAD),
        y: BOX_HEAD + Math.floor(i / columns) * (item + BOX_PAD),
      },
    ]),
  }
}

// One subnet and its boxes, or the resources no subnet on the map holds.
interface Block {
  subnet?: MapCard
  grids: Grid[]
  loose: MapCard[]
  breadth: number
}

// The two axes of a direction: along a tier, and from tier to tier.
interface Axes {
  at: (along: number, deep: number) => MapPlace
  breadth: (width: number, height: number) => number
  depth: (width: number, height: number) => number
}

function axesOf(direction: LayoutDirection): Axes {
  return direction === "across"
    ? {
        at: (along, deep) => ({ x: deep, y: along }),
        breadth: (_, height) => height,
        depth: (width) => width,
      }
    : {
        at: (along, deep) => ({ x: along, y: deep }),
        breadth: (width) => width,
        depth: (_, height) => height,
      }
}

function blocksOf(map: NetworkMap, axes: Axes): Block[] {
  const subnets = map.cards
    .filter((c) => c.tier === "subnets")
    .toSorted(
      (a, b) =>
        Number(a.kind === "more") - Number(b.kind === "more") ||
        (a.column ?? "").localeCompare(b.column ?? "") ||
        a.label.localeCompare(b.label),
    )

  const homes = new Set(subnets.flatMap((s) => s.resourceId ?? []))
  const resources = map.cards.filter((c) => c.tier === "resources")

  const blocks = subnets.map((subnet): Block => {
    const mine = resources.filter(
      (r) => subnet.resourceId !== undefined && r.column === subnet.resourceId,
    )

    const grids = (["instance", "lb"] as const).flatMap((kind) => {
      const cards = mine.filter((c) => c.kind === kind)

      return cards.length > 0 ? [grid(kind, cards)] : []
    })

    return {
      subnet,
      grids,
      loose: [],
      breadth: Math.max(
        axes.breadth(CARD_WIDTH, SUBNET_HEIGHT),
        ...grids.map((g) => axes.breadth(g.width, g.height)),
      ),
    }
  })

  const loose = resources.filter(
    (r) => r.column === undefined || !homes.has(r.column),
  )

  if (loose.length > 0) {
    blocks.push({
      grids: [],
      loose,
      breadth: axes.breadth(CARD_WIDTH, CARD_HEIGHT),
    })
  }

  return blocks
}

function placeBlock(
  block: Block,
  start: number,
  depth: Record<MapTier, number>,
  axes: Axes,
  layout: MapLayout,
) {
  const centred = (breadth: number) => start + (block.breadth - breadth) / 2

  if (block.subnet) {
    layout.places.set(
      block.subnet.id,
      axes.at(centred(axes.breadth(CARD_WIDTH, SUBNET_HEIGHT)), depth.subnets),
    )
  }

  let deep = depth.resources

  for (const g of block.grids) {
    const corner = axes.at(centred(axes.breadth(g.width, g.height)), deep)

    layout.boxes.push({
      id: `box:${g.kind}:${block.subnet?.resourceId ?? ""}`,
      kind: g.kind,
      label: BOX_LABELS[g.kind],
      ...corner,
      width: g.width,
      height: g.height,
    })

    for (const [id, cell] of g.cells) {
      layout.places.set(id, { x: corner.x + cell.x, y: corner.y + cell.y })
    }

    deep += axes.depth(g.width, g.height) + GAP / 2
  }

  const step = axes.depth(CARD_WIDTH, CARD_HEIGHT) + GAP / 2

  for (const [j, card] of block.loose.toSorted(byRankThenName).entries()) {
    layout.places.set(card.id, axes.at(start, depth.resources + j * step))
  }
}

// Hand-placed tiers, top down or left to right: subnets side by side by
// availability zone, each subnet's resources boxed by kind beyond it.
export function layoutNetworkMap(
  map: NetworkMap,
  direction: LayoutDirection = "down",
): MapLayout {
  const axes = axesOf(direction)
  const depth = DEPTH[direction]
  const blocks = blocksOf(map, axes)
  const layout: MapLayout = { places: new Map(), boxes: [] }

  const spread =
    blocks.reduce((sum, b) => sum + b.breadth, 0) +
    GAP * Math.max(blocks.length - 1, 0)

  const step = axes.breadth(CARD_WIDTH, CARD_HEIGHT) + GAP

  const withLabels = new Set(
    map.links.filter((l) => l.label).map((l) => `${l.source}>${l.target}`),
  )

  const joined = (a: string, b: string) =>
    withLabels.has(`${a}>${b}`) || withLabels.has(`${b}>${a}`)

  // Side by side, a labelled line between neighbours needs room for its label.
  const room = direction === "down" ? LABEL_ROOM : 0

  const upper = (["internet", "gateways", "vpc"] as const).map((tier) => {
    const ids = map.cards.filter((c) => c.tier === tier).map((c) => c.id)
    const offsets: number[] = []
    let along = 0

    for (const [i, id] of ids.entries()) {
      const before = ids[i - 1]

      if (before !== undefined) {
        along += step + (joined(before, id) ? room : 0)
      }

      offsets.push(along)
    }

    return {
      tier,
      ids,
      offsets,
      breadth: ids.length > 0 ? along + step - GAP : 0,
    }
  })

  // Every tier is centred on the widest, so none hangs off one side.
  const span = Math.max(spread, ...upper.map((u) => u.breadth))

  for (const { tier, ids, offsets, breadth } of upper) {
    const start = (span - breadth) / 2

    for (const [i, id] of ids.entries()) {
      layout.places.set(id, axes.at(start + (offsets[i] ?? 0), depth[tier]))
    }
  }

  let start = (span - spread) / 2

  for (const block of blocks) {
    placeBlock(block, start, depth, axes, layout)
    start += block.breadth + GAP
  }

  return layout
}

// The cards are sized in rem, so a larger root font needs the layout spread by
// the same factor for the boxes and gaps to still fit them.
export function scaleLayout(layout: MapLayout, unit: number): MapLayout {
  if (unit === 1) {
    return layout
  }

  return {
    places: new Map(
      [...layout.places].map(([id, p]) => [
        id,
        { x: p.x * unit, y: p.y * unit },
      ]),
    ),
    boxes: layout.boxes.map((b) => ({
      ...b,
      x: b.x * unit,
      y: b.y * unit,
      width: b.width * unit,
      height: b.height * unit,
    })),
  }
}
