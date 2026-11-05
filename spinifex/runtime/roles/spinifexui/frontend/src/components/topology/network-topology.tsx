import type { Vpc } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { Handle, type Node, type NodeProps, Position } from "@xyflow/react"
import {
  Cloud,
  Database,
  Ellipsis,
  Globe,
  LayoutGrid,
  type LucideIcon,
  MapPin,
  Network,
  Router,
  Server,
  Waypoints,
} from "lucide-react"
import { useMemo } from "react"

import { ErrorBanner } from "@/components/error-banner"
import {
  InternetCard,
  TopologyCanvas,
  type TopologyEdge,
} from "@/components/topology/topology-canvas"
import {
  buildNetworkMap,
  layoutNetworkMap,
  scaleLayout,
  type MapBox,
  type MapCard,
  type MapFocus,
  type MapKind,
  type MapPlace,
  type MapResource,
} from "@/lib/network-map"
import type { LayoutDirection } from "@/lib/node-topology"
import { cn } from "@/lib/utils"
import { networkMapQueryOptions } from "@/queries/topology"

const ICONS: Record<MapKind, LucideIcon> = {
  internet: Cloud,
  igw: Globe,
  nat: Router,
  eip: MapPin,
  vpc: Network,
  rds: Database,
  subnet: LayoutGrid,
  instance: Server,
  lb: Waypoints,
  more: Ellipsis,
}

// Where a card opens: its list, with the resource's panel selected.
const LISTS = {
  igw: "/ec2/describe-internet-gateways",
  nat: "/ec2/describe-nat-gateways",
  eip: "/ec2/describe-addresses",
  vpc: "/ec2/describe-vpcs",
  rds: "/rds/describe-db-instances",
  subnet: "/ec2/describe-subnets",
  instance: "/ec2/describe-instances",
  lb: "/ec2/describe-load-balancers",
} as const satisfies Partial<Record<MapKind, string>>

type ListedKind = keyof typeof LISTS

function isListed(kind: MapKind): kind is ListedKind {
  return kind in LISTS
}

const NAMES: Record<ListedKind, string> = {
  igw: "internet gateways",
  nat: "NAT gateways",
  eip: "Elastic IPs",
  vpc: "VPCs",
  rds: "databases",
  subnet: "subnets",
  instance: "instances",
  lb: "load balancers",
}

const RESOURCE_NAMES: Record<MapResource, string> = {
  subnets: "subnets",
  routeTables: "route tables",
  internetGateways: "internet gateways",
  natGateways: "NAT gateways",
  addresses: "Elastic IPs",
  instances: "instances",
  loadBalancers: "load balancers",
  dbInstances: "databases",
}

interface CardData extends Record<string, unknown> {
  card: MapCard
  direction: LayoutDirection
}

type CardNode = Node<CardData, "card">

interface BoxData extends Record<string, unknown> {
  box: MapBox
}

type BoxNode = Node<BoxData, "box">

// A line runs from a card's out side to the next tier's in side; a line
// within one tier runs between the cards' neighbouring sides.
const SIDES: Record<
  LayoutDirection,
  { in: Position; out: Position; before: Position; after: Position }
> = {
  down: {
    in: Position.Top,
    out: Position.Bottom,
    before: Position.Left,
    after: Position.Right,
  },
  across: {
    in: Position.Left,
    out: Position.Right,
    before: Position.Top,
    after: Position.Bottom,
  },
}

function Handles({ direction }: { direction: LayoutDirection }) {
  const sides = SIDES[direction]

  return (
    <>
      <Handle
        className="opacity-0"
        isConnectable={false}
        position={sides.in}
        type="target"
      />
      <Handle
        className="opacity-0"
        id="side"
        isConnectable={false}
        position={sides.before}
        type="target"
      />
      <Handle
        className="opacity-0"
        isConnectable={false}
        position={sides.out}
        type="source"
      />
      <Handle
        className="opacity-0"
        id="side"
        isConnectable={false}
        position={sides.after}
        type="source"
      />
    </>
  )
}

// A collapsed tier lists what it holds, each kind linking to its list.
function MoreLinks({ card }: { card: MapCard }) {
  return (
    <ul className="flex flex-col gap-0.5">
      {(card.stands ?? []).map(({ kind, count }) =>
        isListed(kind) ? (
          <li key={kind}>
            <Link
              className="text-2xs text-primary hover:underline"
              to={LISTS[kind]}
            >
              {count} {NAMES[kind]}
            </Link>
          </li>
        ) : undefined,
      )}
    </ul>
  )
}

function Card({ data }: NodeProps<CardNode>) {
  const { card, direction } = data
  const Icon = ICONS[card.kind]

  if (card.kind === "internet") {
    return (
      <InternetCard reach={card.reach ?? "unknown"} testId="card-internet">
        <Handles direction={direction} />
      </InternetCard>
    )
  }

  return (
    <div
      className={cn(
        "flex w-55 items-start gap-2 rounded-lg border bg-card p-2.5 text-card-foreground transition-opacity motion-reduce:transition-none",
        card.kind === "more" && "border-dashed bg-muted",
        isListed(card.kind) && "cursor-pointer hover:border-primary",
        card.highlighted && "border-primary ring-2 ring-primary/30",
        card.dimmed && "opacity-40",
      )}
      data-highlighted={card.highlighted}
      data-testid={`card-${card.id}`}
    >
      <Handles direction={direction} />
      <Icon
        aria-hidden
        className="mt-0.5 size-4 shrink-0 text-muted-foreground"
      />
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="truncate font-mono text-xs font-medium">
          {card.label}
        </span>
        {card.kind === "more" ? (
          <MoreLinks card={card} />
        ) : (
          card.details.map((detail) => (
            <span
              className="truncate font-mono text-2xs text-muted-foreground"
              key={detail}
            >
              {detail}
            </span>
          ))
        )}
        {card.rows && card.rows.length > 0 && (
          <dl className="mt-1 border-t pt-1">
            {card.rows.map((row) => (
              <div
                className="flex justify-between gap-2 font-mono text-2xs"
                key={row.label}
              >
                <dt className="text-muted-foreground">{row.label}</dt>
                <dd className="truncate">{row.value}</dd>
              </div>
            ))}
          </dl>
        )}
      </div>
    </div>
  )
}

// A subnet's resources of one kind, on a fill and dashed border of their own.
function Box({ data }: NodeProps<BoxNode>) {
  const { box } = data

  return (
    <div
      className="size-full rounded-lg border border-dashed border-muted-foreground/40 bg-muted/40 px-3 py-1"
      data-testid={box.id}
    >
      <span className="text-2xs font-medium text-muted-foreground">
        {box.label}
      </span>
    </div>
  )
}

const NODE_TYPES = { card: Card, box: Box }

// The root font size over the 16px the layout is measured in.
function rootUnit(): number {
  const { fontSize } = getComputedStyle(document.documentElement)
  const size = Number(fontSize.replace(/px$/, ""))

  return Number.isFinite(size) && size > 0 ? size / 16 : 1
}

// Along a tier, the earlier of two places: left going down, above going across.
function before(a: MapPlace, b: MapPlace, direction: LayoutDirection) {
  return direction === "down" ? a.x <= b.x : a.y <= b.y
}

function names(resources: MapResource[]): string {
  return resources.map((r) => RESOURCE_NAMES[r]).join(", ")
}

function Notices({
  omitted,
  incomplete,
  failed,
}: {
  omitted: number
  incomplete: MapResource[]
  failed: MapResource[]
}) {
  const lines = [
    omitted > 0 &&
      `The map is truncated: ${omitted} resources are collapsed into "more" cards.`,
    incomplete.length > 0 &&
      `Discovery incomplete for ${names(incomplete)}: there are more than the map reads, so some are not drawn.`,
    failed.length > 0 &&
      `Could not read ${names(failed)}, so they are not drawn.`,
  ].filter((line): line is string => Boolean(line))

  if (lines.length === 0) {
    return null
  }

  return (
    <output className="block rounded-md border border-warning-border bg-warning-subtle p-3 text-xs">
      {lines.map((line) => (
        <p key={line}>{line}</p>
      ))}
    </output>
  )
}

interface NetworkTopologyProps {
  vpc: Vpc
  focus: MapFocus
  direction?: LayoutDirection
}

// A read-only map of one VPC: what reaches the internet, its subnets by zone,
// and what runs in each. A click opens the resource's own panel.
export function NetworkTopology({
  vpc,
  focus,
  direction = "down",
}: NetworkTopologyProps) {
  const vpcId = vpc.VpcId ?? ""
  const { data, error, isPending } = useQuery(networkMapQueryOptions(vpcId))
  const navigate = useNavigate()

  // Keyed on the focus's fields, so a parent's fresh literal does not rebuild
  // the cards, which would make React Flow drop the hover it is showing.
  const { kind, id } = focus

  const map = useMemo(
    () => (data ? buildNetworkMap({ ...data, vpc }, { kind, id }) : undefined),
    [data, vpc, kind, id],
  )

  const layout = useMemo(
    () =>
      map
        ? scaleLayout(layoutNetworkMap(map, direction), rootUnit())
        : undefined,
    [map, direction],
  )

  const nodes = useMemo(() => {
    if (!map || !layout) {
      return []
    }

    // Boxes come first and sit beneath the lines, so they never hide one.
    const boxes = layout.boxes.map((box): BoxNode => ({
      id: box.id,
      type: "box",
      position: { x: box.x, y: box.y },
      width: box.width,
      height: box.height,
      data: { box },
      className: "pointer-events-none",
      draggable: false,
      selectable: false,
      connectable: false,
      zIndex: -1,
    }))

    const cards = map.cards.map((card): CardNode => ({
      id: card.id,
      type: "card",
      position: layout.places.get(card.id) ?? { x: 0, y: 0 },
      data: { card, direction },
      draggable: true,
      connectable: false,
    }))

    return [...boxes, ...cards]
  }, [map, layout, direction])

  const edges = useMemo(() => {
    const tierOf = new Map(map?.cards.map((c) => [c.id, c.tier]))
    const at = (cardId: string) => layout?.places.get(cardId) ?? { x: 0, y: 0 }

    return (map?.links ?? []).map((link): TopologyEdge => {
      const within = tierOf.get(link.source) === tierOf.get(link.target)
      const forward = before(at(link.source), at(link.target), direction)
      const flip = within && !forward

      return {
        id: link.id,
        type: "line",
        source: flip ? link.target : link.source,
        target: flip ? link.source : link.target,
        sourceHandle: within ? "side" : undefined,
        targetHandle: within ? "side" : undefined,
        data: { heavy: link.heavy, label: link.label },
      }
    })
  }, [map, layout, direction])

  if (error) {
    return <ErrorBanner error={error} msg="The VPC's resources did not load." />
  }

  return (
    <div className="flex flex-col gap-3">
      {map && <Notices {...map} />}
      {/* The panel's or page's header takes about 17rem, leaving the legend in view. */}
      <div className="h-[calc(100dvh-17rem)] min-h-96 overflow-hidden rounded-lg border">
        {isPending || !map ? (
          <p className="p-4 text-xs text-muted-foreground">
            Reading the VPC&apos;s resources…
          </p>
        ) : (
          <TopologyCanvas
            edges={edges}
            layoutKey={direction}
            links={map.links}
            nodeTypes={NODE_TYPES}
            nodes={nodes}
            onNodeClick={(node: CardNode | BoxNode) => {
              if (node.type !== "card") {
                return
              }

              const { card } = node.data

              if (isListed(card.kind) && card.resourceId) {
                void navigate({
                  to: LISTS[card.kind],
                  search: { selected: card.resourceId },
                })
              }
            }}
          />
        )}
      </div>
      <p className="text-2xs text-muted-foreground">
        Hover a card to trace its links, or click it to open it.
      </p>
    </div>
  )
}
