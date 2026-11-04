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
  ReactFlow,
} from "@xyflow/react"
import {
  createContext,
  type CSSProperties,
  use,
  useMemo,
  useState,
} from "react"

import { useTheme } from "@/components/theme-provider"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import type { LayoutDirection } from "@/lib/node-topology"
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
  heavy?: boolean
  label?: string
  // Drawn dimmed whatever the hover, for a map that highlights a selection.
  muted?: boolean
}

export type TopologyEdge = Edge<LineData, "line">

type CSSVariables = CSSProperties & Record<`--${string}`, string>

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

  // React Flow gives the label's place in flow coordinates.
  const labelAt: CSSVariables = {
    "--label-x": `${labelX}px`,
    "--label-y": `${labelY}px`,
  }

  return (
    <>
      <BaseEdge
        className={cn(
          "transition-opacity motion-reduce:transition-none",
          data?.heavy
            ? "[--xy-edge-stroke-width:2.5]"
            : "[--xy-edge-stroke-width:1.5]",
          focus === "on" &&
            "[--xy-edge-stroke-width:3] [--xy-edge-stroke:var(--color-primary)]",
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
              "pointer-events-none absolute translate-x-[calc(var(--label-x)-50%)] translate-y-[calc(var(--label-y)-50%)] rounded-sm border bg-background px-1 font-mono text-2xs text-muted-foreground",
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

        return {
          ...edge,
          data: { ...edge.data, focus },
          zIndex: focus === "on" ? 1 : 0,
        }
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
