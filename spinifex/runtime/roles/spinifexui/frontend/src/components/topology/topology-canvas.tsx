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

export interface LineData extends Record<string, unknown> {
  focus?: Focus
  // On the path to the internet, drawn in its own colour.
  heavy?: boolean
  label?: string
  // The label sits against the target, clear of any card the line passes.
  labelAtEnd?: boolean
  // Drawn dimmed whatever the hover, for a map that highlights a selection.
  muted?: boolean
}

export type TopologyEdge = Edge<LineData, "line">

// The gap, in flow px, between a label set at a line's end and its target.
const LABEL_CLEARANCE = 6

type CSSVariables = CSSProperties & Record<`--${string}`, string>

interface LabelPlace {
  x: number
  y: number
  shift: string
}

const CENTRED =
  "translate-x-[calc(var(--label-x)-50%)] translate-y-[calc(var(--label-y)-50%)]"

// Just short of the target, ending at the point rather than centred on it.
function labelAtEnd(x: number, y: number, side: Position): LabelPlace {
  if (side === Position.Left) {
    return {
      x: x - LABEL_CLEARANCE,
      y,
      shift:
        "translate-x-[calc(var(--label-x)-100%)] translate-y-[calc(var(--label-y)-50%)]",
    }
  }

  return {
    x,
    y: y - LABEL_CLEARANCE,
    shift:
      "translate-x-[calc(var(--label-x)-50%)] translate-y-[calc(var(--label-y)-100%)]",
  }
}

export function TopologyLine({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  data,
}: EdgeProps<TopologyEdge>) {
  const [path, labelX, labelY] = getBezierPath({
    sourceX,
    sourceY,
    sourcePosition,
    targetX,
    targetY,
    targetPosition,
  })

  const focus = data?.focus ?? "idle"

  const label = data?.labelAtEnd
    ? labelAtEnd(targetX, targetY, targetPosition)
    : { x: labelX, y: labelY, shift: CENTRED }

  // React Flow gives the label's place in flow coordinates.
  const labelAt: CSSVariables = {
    "--label-x": `${label.x}px`,
    "--label-y": `${label.y}px`,
  }

  return (
    <>
      <BaseEdge
        className={cn(
          "transition-opacity motion-reduce:transition-none",
          data?.heavy
            ? "[--xy-edge-stroke-width:2.5] [--xy-edge-stroke:var(--color-warning)]"
            : "[--xy-edge-stroke-width:1.5]",
          focus === "on" &&
            (data?.heavy
              ? "[--xy-edge-stroke-width:3.5]"
              : "[--xy-edge-stroke-width:3] [--xy-edge-stroke:var(--color-primary)]"),
          (focus === "off" || (data?.muted && focus !== "on")) && "opacity-20",
        )}
        id={id}
        interactionWidth={16}
        path={path}
      />
      {data?.label && (
        <EdgeLabelRenderer>
          <span
            className={cn(
              "pointer-events-none absolute rounded-sm border bg-background px-1 font-mono text-2xs text-muted-foreground",
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
        <Background />
        <Controls showInteractive={false} />
      </ReactFlow>
    </HoverContext>
  )
}
