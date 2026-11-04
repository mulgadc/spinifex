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
  TopologyCanvas,
  type TopologyEdge,
} from "@/components/topology/topology-canvas"
import {
  buildNetworkMap,
  layoutNetworkMap,
  type MapCard,
  type MapFocus,
  type MapKind,
  type MapResource,
} from "@/lib/network-map"
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
  // Cards beside the VPC are joined to its side, not its foot.
  side: "none" | "source" | "target"
}

type CardNode = Node<CardData, "card">

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
  const { card, side } = data
  const Icon = ICONS[card.kind]

  return (
    <div
      className={cn(
        "flex w-55 items-start gap-2 rounded-lg border bg-card p-2.5 text-card-foreground transition-opacity motion-reduce:transition-none",
        card.kind === "internet" && "justify-center rounded-full bg-muted",
        card.kind === "more" && "border-dashed bg-muted",
        isListed(card.kind) && "cursor-pointer hover:border-primary",
        card.highlighted && "border-primary ring-2 ring-primary/30",
        card.dimmed && "opacity-40",
      )}
      data-highlighted={card.highlighted}
      data-testid={`card-${card.id}`}
    >
      <Handle
        className="opacity-0"
        isConnectable={false}
        position={Position.Top}
        type="target"
      />
      {side === "target" && (
        <Handle
          className="opacity-0"
          id="side"
          isConnectable={false}
          position={Position.Left}
          type="target"
        />
      )}
      <Icon
        aria-hidden
        className="mt-0.5 size-4 shrink-0 text-muted-foreground"
      />
      <div className="flex min-w-0 flex-col gap-0.5">
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
      </div>
      <Handle
        className="opacity-0"
        isConnectable={false}
        position={Position.Bottom}
        type="source"
      />
      {side === "source" && (
        <Handle
          className="opacity-0"
          id="side"
          isConnectable={false}
          position={Position.Right}
          type="source"
        />
      )}
    </div>
  )
}

const NODE_TYPES = { card: Card }

function sideOf(card: MapCard): CardData["side"] {
  if (card.kind === "vpc") {
    return "source"
  }

  return card.tier === "vpc" ? "target" : "none"
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

// A read-only map of one VPC: what reaches the internet, its subnets by zone,
// and what runs in each. A click opens the resource's own panel.
export function NetworkTopology({ vpc, focus }: { vpc: Vpc; focus: MapFocus }) {
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

  const nodes = useMemo(() => {
    if (!map) {
      return []
    }

    const places = layoutNetworkMap(map)

    return map.cards.map((card): CardNode => ({
      id: card.id,
      type: "card",
      position: places.get(card.id) ?? { x: 0, y: 0 },
      data: { card, side: sideOf(card) },
      draggable: true,
      connectable: false,
    }))
  }, [map])

  const edges = useMemo(() => {
    const vpcTier = new Set(
      map?.cards.filter((c) => c.tier === "vpc").map((c) => c.id),
    )

    return (map?.links ?? []).map((link): TopologyEdge => {
      const beside = vpcTier.has(link.source) && vpcTier.has(link.target)

      return {
        id: link.id,
        type: "line",
        source: link.source,
        target: link.target,
        sourceHandle: beside ? "side" : undefined,
        targetHandle: beside ? "side" : undefined,
        data: { heavy: link.heavy, label: link.label },
      }
    })
  }, [map])

  if (error) {
    return <ErrorBanner error={error} msg="The VPC's resources did not load." />
  }

  return (
    <div className="flex flex-col gap-3">
      {map && <Notices {...map} />}
      <div className="h-[28rem] overflow-hidden rounded-lg border">
        {isPending || !map ? (
          <p className="p-4 text-xs text-muted-foreground">
            Reading the VPC&apos;s resources…
          </p>
        ) : (
          <TopologyCanvas
            edges={edges}
            links={map.links}
            nodeTypes={NODE_TYPES}
            nodes={nodes}
            onNodeClick={(node: CardNode) => {
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
        A thick line reaches the internet; a labelled line is a route, by its
        destination. Hover a card to trace its links, or click it to open it.
      </p>
    </div>
  )
}
