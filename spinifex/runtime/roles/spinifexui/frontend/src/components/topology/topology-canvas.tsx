import "@xyflow/react/dist/style.css"
import {
  Background,
  BaseEdge,
  Controls,
  type Edge,
  EdgeLabelRenderer,
  type EdgeProps,
  type EdgeTypes,
  getBezierPath,
  type Node,
  type NodeTypes,
  Position,
  ReactFlow,
} from "@xyflow/react"
import {
  CircleCheck,
  CircleQuestionMark,
  CircleX,
  Cloud,
  type LucideIcon,
} from "lucide-react"
import {
  createContext,
  type CSSProperties,
  type ReactNode,
  use,
  useMemo,
  useState,
} from "react"

import { useTheme } from "@/components/theme-provider"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import type { LayoutDirection } from "@/lib/node-topology"
import type { Reachability } from "@/lib/reachability"
import { cn } from "@/lib/utils"

// A hovered card or line lifts what touches it and dims everything else.
export type Focus = "on" | "off" | "idle"

export type Hover = { kind: "node" | "link"; id: string } | undefined

// A line's logical ends, which may differ from the direction it is drawn.
export interface LinkEnds {
  id: string
  source: string
  target: string
}

export function focusOf(link: LinkEnds | undefined, hover: Hover): Focus {
  if (!hover || !link) {
    return "idle"
  }

  const touches =
    hover.kind === "link"
      ? hover.id === link.id
      : hover.id === link.source || hover.id === link.target

  return touches ? "on" : "off"
}

// Hover lives in context, not in node data: a new nodes array makes React
// Flow re-measure every card, which drops the hover it was showing.
const HoverContext = createContext<{
  focus: (linkId: string) => Focus
  setHover: (hover: Hover) => void
}>({
  focus: () => "idle",
  setHover: () => {
    // No canvas, nothing to highlight.
  },
})

export function useTopologyHover() {
  return use(HoverContext)
}

export type LineProtocol = "tcp" | "udp" | "icmp" | "all" | "other"

export interface LineData extends Record<string, unknown> {
  focus?: Focus
  // On the path to the internet, drawn in its own colour.
  heavy?: boolean
  label?: string
  // The label sits against this end, clear of any card the line passes.
  labelEnd?: "source" | "target"
  // Lines sharing an end stack their labels there, and bow apart between.
  slot?: number
  slots?: number
  // A security group rule's protocol, carried by colour and by dash.
  protocol?: LineProtocol
  // Drawn dimmed whatever the hover, for a map that highlights a selection.
  muted?: boolean
  // An arrowhead at the line's middle, pointing from source to target.
  arrow?: boolean
}

export type TopologyEdge = Edge<LineData, "line">

// The gap, in flow px, between a label set at a line's end and its card.
const LABEL_CLEARANCE = 6

// How far, in flow px, each of the lines sharing two ends bows from the next.
const BOW = 14

// Colour and dash together, so a protocol reads without colour.
export const PROTOCOL_LINES: Record<LineProtocol, string> = {
  tcp: "[--xy-edge-stroke:var(--color-chart-1)]",
  udp: "[--xy-edge-stroke:var(--color-chart-2)] [stroke-dasharray:6_4]",
  icmp: "[--xy-edge-stroke:var(--color-chart-3)] [stroke-dasharray:1.5_3] [stroke-linecap:round]",
  all: "[--xy-edge-stroke:var(--color-chart-4)] [stroke-dasharray:10_3_2_3]",
  other:
    "[--xy-edge-stroke:var(--color-muted-foreground)] [stroke-dasharray:2_6]",
}

type CSSVariables = CSSProperties & Record<`--${string}`, string>

interface LabelPlace {
  x: number
  y: number
  // Labels sharing an end stack by this many rows.
  rows: number
  shift: string
}

const CENTRED =
  "translate-x-[calc(var(--label-x)-50%)] translate-y-[calc(var(--label-y)-50%)]"

const END_SHIFTS: Record<Position, string> = {
  [Position.Top]:
    "translate-x-[calc(var(--label-x)-50%)] translate-y-[calc(var(--label-y)-100%-var(--label-rows)*1.25rem)]",
  [Position.Bottom]:
    "translate-x-[calc(var(--label-x)-50%)] translate-y-[calc(var(--label-y)+var(--label-rows)*1.25rem)]",
  [Position.Left]:
    "translate-x-[calc(var(--label-x)-100%)] translate-y-[calc(var(--label-y)-50%+var(--label-rows)*1.25rem)]",
  [Position.Right]:
    "translate-y-[calc(var(--label-y)-50%+var(--label-rows)*1.25rem)] translate-x-(--label-x)",
}

// Just off the card at one end: stacked away from it going down, and centred
// on the line going across.
function labelAtEnd(
  x: number,
  y: number,
  side: Position,
  slot: number,
  slots: number,
): LabelPlace {
  const across = side === Position.Left || side === Position.Right
  const out = side === Position.Bottom || side === Position.Right ? 1 : -1

  return {
    x: across ? x + out * LABEL_CLEARANCE : x,
    y: across ? y : y + out * LABEL_CLEARANCE,
    rows: across ? slot - (slots - 1) / 2 : slot,
    shift: END_SHIFTS[side],
  }
}

type Point = [number, number]

// A curve's control points, bowed off the straight bezier so lines sharing
// both ends part.
function controls(
  [sx, sy]: Point,
  [tx, ty]: Point,
  side: Position,
  bow: number,
): [Point, Point] {
  if (side === Position.Bottom) {
    const d = Math.max(Math.abs(ty - sy) / 2, 20)

    return [
      [sx + bow, sy + d],
      [tx + bow, ty - d],
    ]
  }

  const d = Math.max(Math.abs(tx - sx) / 2, 20)

  return [
    [sx + d, sy + bow],
    [tx - d, ty + bow],
  ]
}

const mid = (a: Point, b: Point): Point => [
  (a[0] + b[0]) / 2,
  (a[1] + b[1]) / 2,
]

const at = (p: Point) => `${p[0]},${p[1]}`

function bowedPath(
  source: Point,
  target: Point,
  side: Position,
  bow: number,
): string {
  const [c1, c2] = controls(source, target, side, bow)

  return `M ${at(source)} C ${at(c1)} ${at(c2)} ${at(target)}`
}

// The same curve split in two at its middle, where a mid marker then sits.
function arrowedPath(
  source: Point,
  target: Point,
  side: Position,
  bow: number,
): string {
  const [c1, c2] = controls(source, target, side, bow)
  const [a, b, c] = [mid(source, c1), mid(c1, c2), mid(c2, target)]
  const [d, e] = [mid(a, b), mid(b, c)]
  const m = mid(d, e)

  return `M ${at(source)} C ${at(a)} ${at(d)} ${at(m)} C ${at(e)} ${at(c)} ${at(target)}`
}

export const ARROW_MARKER = "topology-arrow"

// Filled with each line's own stroke, so an arrow takes its protocol's colour.
function ArrowMarker() {
  return (
    <svg aria-hidden className="absolute size-0">
      <defs>
        <marker
          id={ARROW_MARKER}
          markerHeight={6}
          markerWidth={6}
          orient="auto"
          refX={5}
          refY={5}
          viewBox="0 0 10 10"
        >
          <path d="M 0 0 L 10 5 L 0 10 z" fill="context-stroke" />
        </marker>
      </defs>
    </svg>
  )
}

function placeLabel(
  props: EdgeProps<TopologyEdge>,
  middle: [number, number],
): LabelPlace {
  const { data } = props
  const slot = data?.slot ?? 0
  const slots = data?.slots ?? 1

  if (data?.labelEnd === "source") {
    return labelAtEnd(
      props.sourceX,
      props.sourceY,
      props.sourcePosition,
      slot,
      slots,
    )
  }

  if (data?.labelEnd === "target") {
    return labelAtEnd(
      props.targetX,
      props.targetY,
      props.targetPosition,
      slot,
      slots,
    )
  }

  return { x: middle[0], y: middle[1], rows: 0, shift: CENTRED }
}

function lineClass(data: LineData | undefined, focus: Focus): string {
  const lifted = focus === "on"
  const faded = focus === "off" || (data?.muted && !lifted)

  if (data?.protocol) {
    return cn(
      PROTOCOL_LINES[data.protocol],
      lifted ? "[--xy-edge-stroke-width:3]" : "[--xy-edge-stroke-width:1.75]",
      faded && "opacity-20",
    )
  }

  return cn(
    data?.heavy
      ? "[--xy-edge-stroke-width:2.5] [--xy-edge-stroke:var(--color-warning)]"
      : "[--xy-edge-stroke-width:1.5]",
    lifted &&
      (data?.heavy
        ? "[--xy-edge-stroke-width:3.5]"
        : "[--xy-edge-stroke-width:3] [--xy-edge-stroke:var(--color-primary)]"),
    faded && "opacity-20",
  )
}

export function TopologyLine(props: EdgeProps<TopologyEdge>) {
  const {
    id,
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
    data,
  } = props

  const [curve, labelX, labelY] = getBezierPath({
    sourceX,
    sourceY,
    sourcePosition,
    targetX,
    targetY,
    targetPosition,
  })

  const bow = ((data?.slot ?? 0) - ((data?.slots ?? 1) - 1) / 2) * BOW

  const ends: [Point, Point] = [
    [sourceX, sourceY],
    [targetX, targetY],
  ]

  let path = curve

  if (data?.arrow) {
    path = arrowedPath(...ends, sourcePosition, bow)
  } else if (bow !== 0) {
    path = bowedPath(...ends, sourcePosition, bow)
  }

  const focus = data?.focus ?? "idle"
  const label = placeLabel(props, [labelX, labelY])

  // React Flow gives the label's place in flow coordinates.
  const labelAt: CSSVariables = {
    "--label-x": `${label.x}px`,
    "--label-y": `${label.y}px`,
    "--label-rows": `${label.rows}`,
  }

  return (
    <>
      <BaseEdge
        className={cn(
          "transition-opacity motion-reduce:transition-none",
          lineClass(data, focus),
        )}
        id={id}
        interactionWidth={16}
        markerMid={data?.arrow ? `url(#${ARROW_MARKER})` : undefined}
        path={path}
      />
      {data?.label && (
        <EdgeLabelRenderer>
          <span
            className={cn(
              "pointer-events-none absolute rounded-sm border bg-background px-1 font-mono text-2xs whitespace-nowrap text-muted-foreground",
              label.shift,
              (focus === "off" || (data.muted && focus !== "on")) &&
                "opacity-30",
            )}
            data-testid={`line-label-${id}`}
            style={labelAt}
          >
            {data.label}
          </span>
        </EdgeLabelRenderer>
      )}
    </>
  )
}

export function DirectionToggle({
  value,
  onChange,
}: {
  value: LayoutDirection
  onChange: (direction: LayoutDirection) => void
}) {
  return (
    <ToggleGroup
      aria-label="Layout"
      onValueChange={(next: string[]) => {
        const [picked] = next

        if (picked === "down" || picked === "across") {
          onChange(picked)
        }
      }}
      value={[value]}
      variant="outline"
    >
      <ToggleGroupItem value="across">Left to right</ToggleGroupItem>
      <ToggleGroupItem value="down">Top down</ToggleGroupItem>
    </ToggleGroup>
  )
}

const REACH: Record<
  Reachability,
  { icon: LucideIcon; text: string; card: string; mark: string }
> = {
  reachable: {
    icon: CircleCheck,
    text: "Internet access",
    card: "border-success ring-2 ring-success/30",
    mark: "text-success",
  },
  unreachable: {
    icon: CircleX,
    text: "No internet access",
    card: "border-destructive ring-2 ring-destructive/30",
    mark: "text-destructive",
  },
  unknown: {
    icon: CircleQuestionMark,
    text: "Reachability unknown",
    card: "border-dashed",
    mark: "text-muted-foreground",
  },
}

// The Internet card both topologies draw: green when reached, red with a
// cross when not, grey when what decides it was not read.
export function InternetCard({
  reach,
  testId,
  children,
}: {
  reach: Reachability
  testId?: string
  // The card's handles.
  children?: ReactNode
}) {
  const { icon: Mark, text, card, mark } = REACH[reach]

  return (
    <div
      className={cn(
        "flex w-48 items-center justify-center gap-2 rounded-full border bg-muted px-3 py-2.5 text-card-foreground",
        card,
      )}
      data-reach={reach}
      data-testid={testId}
    >
      {children}
      <Cloud aria-hidden className="size-5 shrink-0 text-muted-foreground" />
      <div className="flex min-w-0 flex-col">
        <span className="font-mono text-xs font-medium">Internet</span>
        <span className="flex items-center gap-1 text-2xs text-muted-foreground">
          <Mark aria-hidden className={cn("size-3 shrink-0", mark)} />
          {text}
        </span>
      </div>
    </div>
  )
}

const EDGE_TYPES: EdgeTypes = { line: TopologyLine }

interface TopologyCanvasProps<N extends Node> {
  nodes: N[]
  // Each line's id names one of links, whose ends decide what a hover lifts.
  edges: TopologyEdge[]
  links: readonly LinkEnds[]
  // The card renderers, keyed by node type.
  nodeTypes: NodeTypes
  onNodeClick?: (node: N) => void
  // A new layout remounts the flow, so it fits the view again.
  layoutKey?: string
}

// The React Flow shell every topology shares: themed, fitted, read-only, with
// zoom controls, no attribution, and hover tracing across cards and lines.
// A lifted line still draws beneath every card.
export function TopologyCanvas<N extends Node>({
  nodes,
  edges,
  links,
  nodeTypes,
  onNodeClick,
  layoutKey,
}: TopologyCanvasProps<N>) {
  const { theme } = useTheme()
  const [hover, setHover] = useState<Hover>()

  const byId = useMemo(
    () => new Map(links.map((link) => [link.id, link])),
    [links],
  )

  const hoverContext = useMemo(
    () => ({
      focus: (linkId: string) => focusOf(byId.get(linkId), hover),
      setHover,
    }),
    [byId, hover],
  )

  const focused = useMemo(
    () =>
      edges.map((edge): TopologyEdge => {
        const focus = focusOf(byId.get(edge.id), hover)

        return { ...edge, data: { ...edge.data, focus } }
      }),
    [edges, byId, hover],
  )

  return (
    <HoverContext value={hoverContext}>
      <ReactFlow
        colorMode={theme === "dark" ? "dark" : "light"}
        edgeTypes={EDGE_TYPES}
        edges={focused}
        fitView
        key={layoutKey}
        // Low enough for a wide map to fit a side panel whole.
        minZoom={0.2}
        nodeTypes={nodeTypes}
        nodes={nodes}
        nodesConnectable={false}
        onEdgeMouseEnter={(_, edge) => {
          setHover({ kind: "link", id: edge.id })
        }}
        onEdgeMouseLeave={() => {
          setHover(undefined)
        }}
        onNodeClick={(_, node) => {
          onNodeClick?.(node)
        }}
        onNodeMouseEnter={(_, node) => {
          setHover({ kind: "node", id: node.id })
        }}
        onNodeMouseLeave={() => {
          setHover(undefined)
        }}
        proOptions={{ hideAttribution: true }}
      >
        <ArrowMarker />
        <Background />
        <Controls showInteractive={false} />
      </ReactFlow>
    </HoverContext>
  )
}
