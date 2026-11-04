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

import { routeTableFor } from "@/lib/subnet-routing"

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

export interface MapCard {
  id: string
  kind: MapKind
  tier: MapTier
  label: string
  details: string[]
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

export interface MapFocus {
  kind: "vpc"
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

function sketchNatGateways(sketch: Sketch, nats: NatGateway[]) {
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
    const table = routeTableFor(subnet, routeTables)

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
      details: compact([...named.details, instance.PrivateIpAddress]),
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
  const sketch: Sketch = { vpcId, cards: [], links: [] }

  const nats = inputs.natGateways.filter(
    (nat) => nat.VpcId === vpcId && LIVE_NAT.has(nat.State ?? ""),
  )

  const instances = inputs.instances.filter(
    (i) => i.VpcId === vpcId && !GONE_INSTANCE.has(i.State?.Name ?? ""),
  )

  sketchFrame(sketch, inputs.vpc)
  sketchInternetGateways(sketch, inputs.internetGateways)
  sketchNatGateways(sketch, nats)
  sketchSubnets(
    sketch,
    inputs.subnets.filter((subnet) => subnet.VpcId === vpcId),
    inputs.routeTables,
  )
  sketchInstances(sketch, instances)
  sketchLoadBalancers(sketch, inputs.loadBalancers)
  sketchDatabases(sketch, inputs.dbInstances)
  sketchAddresses(sketch, inputs.addresses, nats, instances)

  return sketch
}

// The order cards claim the budget: the selection, the map's frame, the
// selection's neighbours, then everything else, each kind sorted by name.
function claimOrder(cards: Draft[], links: MapLink[], selected: string) {
  const frame = cards
    .filter((c) => ["internet", "vpc", "igw", "nat"].includes(c.kind))
    .toSorted(byRankThenName)

  const touching = new Set(
    links.flatMap((l) => {
      if (l.source === selected) {
        return [l.target]
      }

      return l.target === selected ? [l.source] : []
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
  const { cards, links } = sketchMap(inputs)
  const vpcCard = `vpc:${inputs.vpc.VpcId ?? ""}`
  const selected = `${focus.kind}:${focus.id}`
  // One card and one line in each tier are kept back for its "+N more".
  const cardRoom = budget.cards - MAP_TIERS.length
  const edgeRoom = budget.edges - MAP_TIERS.length

  const ends = new Map<string, MapLink[]>()

  for (const l of links) {
    ends.set(l.source, [...(ends.get(l.source) ?? []), l])

    if (l.target !== l.source) {
      ends.set(l.target, [...(ends.get(l.target) ?? []), l])
    }
  }

  const placed = new Set<string>()
  let lines = 0

  for (const id of claimOrder(cards, links, selected)) {
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
  const highlight = new Set([selected])

  return {
    cards: [...kept, ...more].map((card) => ({
      ...card,
      highlighted: highlight.has(card.id),
      dimmed: false,
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

export const CARD_WIDTH = 220
const GAP = 32
const STEP = CARD_WIDTH + GAP

const TIER_Y: Record<MapTier, number> = {
  internet: 0,
  gateways: 130,
  vpc: 270,
  subnets: 410,
  resources: 560,
}

const RESOURCE_STEP = 100

function row(ids: string[], width: number, y: number): [string, MapPlace][] {
  const start = (width - (ids.length * STEP - GAP)) / 2

  return ids.map((id, i) => [id, { x: start + i * STEP, y }])
}

function stack(cards: MapCard[], x: number, places: Map<string, MapPlace>) {
  for (const [j, card] of cards.toSorted(byRankThenName).entries()) {
    places.set(card.id, { x, y: TIER_Y.resources + j * RESOURCE_STEP })
  }
}

// Hand-placed tiers, top down: subnets in columns by availability zone, and
// each subnet's resources stacked beneath it.
export function layoutNetworkMap(map: NetworkMap): Map<string, MapPlace> {
  const of = (tier: MapTier) => map.cards.filter((c) => c.tier === tier)

  const subnets = of("subnets").toSorted(
    (a, b) =>
      Number(a.kind === "more") - Number(b.kind === "more") ||
      (a.column ?? "").localeCompare(b.column ?? "") ||
      a.label.localeCompare(b.label),
  )

  const homes = new Set(subnets.flatMap((s) => s.resourceId ?? []))
  const resources = of("resources")

  const loose = resources.filter(
    (r) => r.column === undefined || !homes.has(r.column),
  )

  const columns = subnets.length + (loose.length > 0 ? 1 : 0)

  // Every row is centred on the widest, so no tier hangs off one side.
  const widest = Math.max(columns, of("gateways").length, of("vpc").length, 1)

  const width = widest * STEP - GAP
  const left = (width - (columns * STEP - GAP)) / 2
  const places = new Map<string, MapPlace>()

  for (const tier of ["internet", "gateways", "vpc"] as const) {
    const ids = of(tier).map((c) => c.id)

    for (const [id, place] of row(ids, width, TIER_Y[tier])) {
      places.set(id, place)
    }
  }

  for (const [i, subnet] of subnets.entries()) {
    const x = left + i * STEP

    places.set(subnet.id, { x, y: TIER_Y.subnets })

    const home = subnet.resourceId

    if (home !== undefined) {
      stack(
        resources.filter((r) => r.column === home),
        x,
        places,
      )
    }
  }

  stack(loose, left + subnets.length * STEP, places)

  return places
}
