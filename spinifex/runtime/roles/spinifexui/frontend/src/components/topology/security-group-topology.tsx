import type { SecurityGroup } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import type { Node, NodeProps } from "@xyflow/react"
import {
  Database,
  Ellipsis,
  Globe,
  List,
  type LucideIcon,
  Server,
  Shield,
  Waypoints,
} from "lucide-react"
import { useMemo } from "react"

import { ErrorBanner } from "@/components/error-banner"
import {
  Handles,
  Notices,
  rootUnit,
} from "@/components/topology/network-topology"
import {
  type LineProtocol,
  PROTOCOL_LINES,
  TopologyCanvas,
  type TopologyEdge,
} from "@/components/topology/topology-canvas"
import type { LayoutDirection } from "@/lib/node-topology"
import {
  type AmongRule,
  buildSecurityGroupMap,
  type GroupBox,
  type GroupCard,
  type GroupCardKind,
  layoutSecurityGroupMap,
  scaleGroupLayout,
} from "@/lib/security-group-map"
import { cn } from "@/lib/utils"
import { securityGroupMapQueryOptions } from "@/queries/topology"

const ICONS: Record<GroupCardKind, LucideIcon> = {
  group: Shield,
  peer: Shield,
  cidr: Globe,
  prefix: List,
  instance: Server,
  lb: Waypoints,
  rds: Database,
  more: Ellipsis,
}

// Where a card opens: its list, with the resource's panel selected.
const LISTS = {
  group: "/ec2/describe-security-groups",
  peer: "/ec2/describe-security-groups",
  instance: "/ec2/describe-instances",
  lb: "/ec2/describe-load-balancers",
  rds: "/rds/describe-db-instances",
} as const satisfies Partial<Record<GroupCardKind, string>>

type ListedKind = keyof typeof LISTS

function isListed(kind: GroupCardKind): kind is ListedKind {
  return kind in LISTS
}

const LEGEND: { protocol: LineProtocol; label: string }[] = [
  { protocol: "tcp", label: "TCP" },
  { protocol: "udp", label: "UDP" },
  { protocol: "icmp", label: "ICMP" },
  { protocol: "all", label: "All traffic" },
  { protocol: "other", label: "Other" },
]

interface CardData extends Record<string, unknown> {
  card: GroupCard
  direction: LayoutDirection
}

type CardNode = Node<CardData, "card">

interface BoxData extends Record<string, unknown> {
  box: GroupBox
  among: AmongRule[]
}

type BoxNode = Node<BoxData, "box">

interface CaptionData extends Record<string, unknown> {
  side: "inbound" | "outbound"
  direction: LayoutDirection
}

type CaptionNode = Node<CaptionData, "caption">

const CAPTIONS = {
  inbound: { title: "Inbound", note: "traffic allowed in" },
  outbound: { title: "Outbound", note: "traffic allowed out" },
}

function Card({ data }: NodeProps<CardNode>) {
  const { card, direction } = data
  const Icon = ICONS[card.kind]

  return (
    <div
      className={cn(
        "flex w-55 items-start gap-2 rounded-lg border bg-card p-2.5 text-card-foreground",
        card.kind === "more" && "border-dashed bg-muted",
        isListed(card.kind) && "cursor-pointer hover:border-primary",
        card.highlighted && "border-primary ring-2 ring-primary/30",
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
        {card.details.map((detail) => (
          <span
            className="truncate font-mono text-2xs text-muted-foreground"
            key={detail}
          >
            {detail}
          </span>
        ))}
      </div>
    </div>
  )
}

// The group's members, on a fill and dashed border of their own, headed by
// the rules that let them reach each other.
function Box({ data }: NodeProps<BoxNode>) {
  return (
    <div
      className="flex size-full flex-col rounded-lg border border-dashed border-muted-foreground/40 bg-muted/40 px-3 py-1 text-2xs text-muted-foreground"
      data-testid="box:members"
    >
      <span className="h-4 font-medium">Members</span>
      {data.among.length > 0 && (
        <ul aria-label="Between members" data-testid="among">
          <li className="h-4">Members reach each other on:</li>
          {data.among.map((rule) => (
            <li className="h-4 truncate font-mono" key={rule.label}>
              {rule.label}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function Caption({ data }: NodeProps<CaptionNode>) {
  const { title, note } = CAPTIONS[data.side]

  return (
    <div
      className={cn(
        "flex w-30 flex-col text-2xs text-muted-foreground",
        data.direction === "down" && "text-right",
      )}
      data-testid={`caption:${data.side}`}
    >
      <span className="text-xs font-medium text-foreground">{title}</span>
      <span>{note}</span>
    </div>
  )
}

const NODE_TYPES = { card: Card, box: Box, caption: Caption }

function Legend() {
  return (
    <ul
      aria-label="Rule protocols"
      className="flex flex-wrap gap-x-4 gap-y-1 text-2xs text-muted-foreground"
    >
      {LEGEND.map(({ protocol, label }) => (
        <li className="flex items-center gap-1.5" key={protocol}>
          <svg aria-hidden className="h-2 w-6" viewBox="0 0 24 8">
            <path
              className={cn(
                PROTOCOL_LINES[protocol],
                "fill-none stroke-(--xy-edge-stroke) stroke-2",
              )}
              d="M 0 4 L 24 4"
            />
          </svg>
          {label}
        </li>
      ))}
    </ul>
  )
}

interface SecurityGroupTopologyProps {
  group: SecurityGroup
  direction?: LayoutDirection
}

// A read-only map of one security group: inbound rules' sources, the group
// with what it covers, and outbound rules' destinations, one line per rule.
export function SecurityGroupTopology({
  group,
  direction = "down",
}: SecurityGroupTopologyProps) {
  const { data, error, isPending } = useQuery(
    securityGroupMapQueryOptions(group),
  )

  const navigate = useNavigate()

  const map = useMemo(
    () => (data ? buildSecurityGroupMap({ ...data, group }) : undefined),
    [data, group],
  )

  const layout = useMemo(
    () =>
      map
        ? scaleGroupLayout(layoutSecurityGroupMap(map, direction), rootUnit())
        : undefined,
    [map, direction],
  )

  const nodes = useMemo(() => {
    if (!map || !layout) {
      return []
    }

    const box: BoxNode[] = layout.members
      ? [
          {
            id: "box:members",
            type: "box",
            position: { x: layout.members.x, y: layout.members.y },
            width: layout.members.width,
            height: layout.members.height,
            data: { box: layout.members, among: map.among },
            className: "pointer-events-none",
            draggable: false,
            selectable: false,
            connectable: false,
            zIndex: -1,
          },
        ]
      : []

    const cards = map.cards.map((card): CardNode => ({
      id: card.id,
      type: "card",
      position: layout.places.get(card.id) ?? { x: 0, y: 0 },
      data: { card, direction },
      draggable: true,
      connectable: false,
    }))

    const captions = [...layout.captions].map(([side, place]): CaptionNode => ({
      id: `caption:${side}`,
      type: "caption",
      position: place,
      data: { side, direction },
      className: "pointer-events-none",
      draggable: false,
      selectable: false,
      connectable: false,
    }))

    return [...box, ...captions, ...cards]
  }, [map, layout, direction])

  const edges = useMemo(
    () =>
      (map?.links ?? []).map((link): TopologyEdge => ({
        id: link.id,
        type: "line",
        source: link.source,
        target: link.target,
        data: {
          label: link.label,
          protocol: link.protocol,
          // Each rule's label sits by its source or destination, not the group.
          labelEnd: link.target.startsWith("group:") ? "source" : "target",
          slot: link.slot,
          slots: link.slots,
          arrow: true,
        },
      })),
    [map],
  )

  if (error) {
    return (
      <ErrorBanner error={error} msg="The security group's map did not load." />
    )
  }

  return (
    <div className="flex flex-col gap-3">
      {map && <Notices {...map} />}
      {/* The panel's or page's header takes about 17rem, and the legend and hint 3rem more. */}
      <div className="h-[calc(100dvh-20rem)] min-h-96 overflow-hidden rounded-lg border">
        {isPending || !map ? (
          <p className="p-4 text-xs text-muted-foreground">
            Reading what the group covers…
          </p>
        ) : (
          <TopologyCanvas
            edges={edges}
            layoutKey={direction}
            links={map.links}
            nodeTypes={NODE_TYPES}
            nodes={nodes}
            onNodeClick={(node: CardNode | BoxNode | CaptionNode) => {
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
      <Legend />
      <p className="text-2xs text-muted-foreground">
        Arrows run the way traffic is allowed. Hover a card to trace its rules,
        or click it to open it. A rule naming a group matches that group&apos;s
        members by private address only.
      </p>
    </div>
  )
}
