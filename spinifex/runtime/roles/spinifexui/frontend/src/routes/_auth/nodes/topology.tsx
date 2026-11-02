import "@xyflow/react/dist/style.css"
import { useQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import {
  Background,
  BaseEdge,
  Controls,
  type Edge,
  type EdgeProps,
  getBezierPath,
  Handle,
  type Node,
  type NodeProps,
  Position,
  ReactFlow,
} from "@xyflow/react"
import { createContext, use, useMemo, useState } from "react"

import { DeviceArt } from "@/components/device-art"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { useTheme } from "@/components/theme-provider"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  buildTopology,
  formatLinkSpeed,
  type LayoutDirection,
  layoutTopology,
  planesLabel,
  type TopologyDevice,
  type TopologyLink,
} from "@/lib/node-topology"
import { requireAdmin } from "@/lib/require-admin"
import { cn } from "@/lib/utils"
import { type NICFacts, nodeFactsQueryOptions } from "@/queries/nodes"

export const Route = createFileRoute("/_auth/nodes/topology")({
  beforeLoad: requireAdmin,
  head: () => ({
    meta: [{ title: "Topology | Nodes | Mulga" }],
  }),
  component: TopologyPage,
})

// A hovered link lifts its line and its port row and dims every other link.
type Focus = "on" | "off" | "idle"

type Hover = { kind: "node" | "link"; id: string } | undefined

interface DeviceData extends Record<string, unknown> {
  device: TopologyDevice
  direction: LayoutDirection
  // A node lists its NICs, a switch the speeds of the ports that reach it.
  nics: NICFacts[]
  speeds: string[]
}

// Hover lives in context, not in node data: a new nodes array makes React
// Flow re-measure every card, which drops the hover it was showing.
const HoverContext = createContext<{
  focus: (linkId: string) => Focus
  setHover: (hover: Hover) => void
}>({
  focus: () => "idle",
  setHover: () => {
    // No provider, nothing to highlight.
  },
})

interface LinkData extends Record<string, unknown> {
  focus: Focus
  wan: boolean
}

type DeviceNode = Node<DeviceData, "device">

type LinkEdge = Edge<LinkData, "link">

const SIDES: Record<LayoutDirection, { in: Position; out: Position }> = {
  down: { in: Position.Top, out: Position.Bottom },
  across: { in: Position.Left, out: Position.Right },
}

// Where each of a card's top handles sits, by how many linked ports it has;
// literal classes, so Tailwind sees them. Past five they share the middle.
const PORT_AT: readonly (readonly string[])[] = [
  [],
  ["left-1/2"],
  ["left-1/3", "left-2/3"],
  ["left-1/4", "left-1/2", "left-3/4"],
  ["left-1/5", "left-2/5", "left-3/5", "left-4/5"],
  ["left-1/6", "left-2/6", "left-3/6", "left-4/6", "left-5/6"],
]

function isLinked(nic: NICFacts): boolean {
  return (nic.planes?.length ?? 0) > 0
}

function PortRow({
  nic,
  linkId,
  data,
}: {
  nic: NICFacts
  linkId: string
  data: DeviceData
}) {
  const hover = use(HoverContext)
  const linked = isLinked(nic)
  const focus = linked ? hover.focus(linkId) : "idle"

  return (
    <div
      className={cn(
        "relative -mx-3 flex items-center justify-between gap-3 px-3 py-1 font-mono text-2xs",
        !linked && "text-muted-foreground opacity-60",
        focus === "on" && "bg-primary/10 text-foreground",
        focus === "off" && "opacity-40",
      )}
      data-focus={focus}
      data-testid={`port-${linkId}`}
      onMouseEnter={() => {
        if (linked) {
          hover.setHover({ kind: "link", id: linkId })
        }
      }}
      onMouseLeave={() => {
        hover.setHover({ kind: "node", id: data.device.id })
      }}
    >
      {linked && data.direction === "across" && (
        <Handle
          className="opacity-0"
          id={nic.name}
          isConnectable={false}
          position={Position.Left}
          type="target"
        />
      )}
      <span className="min-w-0 truncate font-medium">
        {nic.name}
        {nic.neighbor?.port_id && (
          <span className="font-normal text-muted-foreground">
            {` ↔ ${nic.neighbor.port_id}`}
          </span>
        )}
      </span>
      <span className="shrink-0 text-muted-foreground">
        {linked
          ? `${formatLinkSpeed(nic.speed_mbps)} · ${planesLabel(nic.planes ?? [])}`
          : nic.state}
      </span>
    </div>
  )
}

// Across, each link ends on its port's row. Down, the card's top edge carries
// one handle per linked port, spread evenly, in the rows' order.
function NodeCard({ data }: { data: DeviceData }) {
  const { device } = data
  const linked = data.nics.filter(isLinked)

  return (
    <div className="w-88 cursor-pointer rounded-lg border bg-card p-3 text-card-foreground hover:border-primary">
      {data.direction === "down" &&
        linked.map((nic, index) => (
          <Handle
            className={cn(
              "opacity-0",
              PORT_AT[linked.length]?.[index] ?? "left-1/2",
            )}
            id={nic.name}
            isConnectable={false}
            key={nic.name}
            position={Position.Top}
            type="target"
          />
        ))}
      <div className="mb-2 flex items-center gap-3">
        <DeviceArt
          className="h-10 w-14 shrink-0 text-muted-foreground"
          formFactor={device.formFactor}
          kind={device.kind}
        />
        <div className="flex min-w-0 flex-col gap-0.5">
          <span className="truncate font-mono text-xs font-medium">
            {device.label}
          </span>
          {device.detail && (
            <span className="truncate text-2xs text-muted-foreground">
              {device.detail}
            </span>
          )}
        </div>
      </div>
      <div className="border-t pt-1">
        {data.nics.map((nic) => (
          <PortRow
            data={data}
            key={nic.name}
            linkId={`${device.label}:${nic.name}`}
            nic={nic}
          />
        ))}
      </div>
    </div>
  )
}

// Switches and the internet sit on a muted fill, so infrastructure reads apart
// from the hosts; a network with no LLDP neighbour is dashed.
function DeviceCard({ data }: NodeProps<DeviceNode>) {
  const { device } = data
  const sides = SIDES[data.direction]

  if (device.kind === "node") {
    return <NodeCard data={data} />
  }

  return (
    <div
      className={cn(
        "flex w-56 items-center gap-3 rounded-lg border bg-muted p-3 text-card-foreground",
        device.kind === "segment" && "border-dashed",
        device.kind === "internet" && "w-40 justify-center rounded-full",
      )}
    >
      <Handle
        className="opacity-0"
        isConnectable={false}
        position={sides.in}
        type="target"
      />
      <DeviceArt
        className="h-8 w-11 shrink-0 text-muted-foreground"
        formFactor={device.formFactor}
        kind={device.kind}
      />
      <div className="flex min-w-0 flex-col gap-0.5">
        <span className="truncate font-mono text-xs font-medium">
          {device.label}
        </span>
        {device.detail && (
          <span className="truncate text-2xs text-muted-foreground">
            {device.detail}
          </span>
        )}
        {data.speeds.length > 0 && (
          <span className="text-2xs text-muted-foreground">
            {data.speeds.length} port{data.speeds.length === 1 ? "" : "s"} ·{" "}
            {[...new Set(data.speeds)].join(", ")}
          </span>
        )}
        {device.kind !== "internet" && device.planes.length > 0 && (
          <span className="text-2xs text-muted-foreground">
            {planesLabel(device.planes)}
          </span>
        )}
      </div>
      <Handle
        className="opacity-0"
        isConnectable={false}
        position={sides.out}
        type="source"
      />
    </div>
  )
}

function LinkLine({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  data,
}: EdgeProps<LinkEdge>) {
  const [path] = getBezierPath({
    sourceX,
    sourceY,
    sourcePosition,
    targetX,
    targetY,
    targetPosition,
  })

  const focus = data?.focus ?? "idle"

  return (
    <BaseEdge
      className={cn(
        "transition-opacity motion-reduce:transition-none",
        data?.wan
          ? "[--xy-edge-stroke-width:2.5]"
          : "[--xy-edge-stroke-width:1.5]",
        focus === "on" &&
          "[--xy-edge-stroke-width:3] [--xy-edge-stroke:var(--color-primary)]",
        focus === "off" && "opacity-20",
      )}
      id={id}
      interactionWidth={16}
      path={path}
    />
  )
}

function focusOf(link: TopologyLink | undefined, hover: Hover): Focus {
  if (!hover || !link) {
    return "idle"
  }

  const touches =
    hover.kind === "link"
      ? hover.id === link.id
      : hover.id === link.source || hover.id === link.target

  return touches ? "on" : "off"
}

const NODE_TYPES = { device: DeviceCard }
const EDGE_TYPES = { link: LinkLine }

function TopologyPage() {
  const { data, error, isPending } = useQuery(nodeFactsQueryOptions)
  const { theme } = useTheme()
  const navigate = useNavigate()
  const [direction, setDirection] = useState<LayoutDirection>("across")
  const [hover, setHover] = useState<Hover>()

  const facts = useMemo(() => data?.nodes ?? [], [data])
  const topology = useMemo(() => buildTopology(facts), [facts])

  const placed = useMemo(
    () =>
      new Map(
        layoutTopology(topology, direction).map((place) => [place.id, place]),
      ),
    [topology, direction],
  )

  const nodes = useMemo(() => {
    const speeds = new Map<string, string[]>()

    for (const link of topology.links) {
      if (link.nic) {
        speeds.set(link.target, [
          ...(speeds.get(link.target) ?? []),
          formatLinkSpeed(link.speedMbps),
        ])
      }
    }

    return topology.devices.map((device): DeviceNode => ({
      id: device.id,
      type: "device",
      position: {
        x: placed.get(device.id)?.x ?? 0,
        y: placed.get(device.id)?.y ?? 0,
      },
      data: {
        device,
        direction,
        nics:
          device.kind === "node"
            ? (facts.find((f) => `node:${f.node}` === device.id)?.nics ?? [])
            : [],
        speeds: speeds.get(device.id) ?? [],
      },
      draggable: true,
      connectable: false,
    }))
  }, [topology, facts, placed, direction])

  const hoverContext = useMemo(() => {
    const byId = new Map(topology.links.map((link) => [link.id, link]))

    return {
      focus: (linkId: string) => focusOf(byId.get(linkId), hover),
      setHover,
    }
  }, [topology, hover])

  // A node link runs from its switch into the node's own port row; the
  // internet's links run into the switches.
  const edges = useMemo(
    () =>
      topology.links.map((link): LinkEdge => {
        const fromNode = link.source.startsWith("node:")
        // Both kinds are recorded upstream-last, so draw them reversed.
        const flip = fromNode || link.target === "internet"

        const focus = focusOf(link, hover)

        return {
          id: link.id,
          type: "link",
          source: flip ? link.target : link.source,
          target: flip ? link.source : link.target,
          targetHandle: fromNode ? link.nic : undefined,
          data: { focus, wan: link.planes.includes("wan") },
          zIndex: focus === "on" ? 1 : 0,
        }
      }),
    [topology, hover],
  )

  return (
    <>
      <PageHeading
        actions={
          <ToggleGroup
            aria-label="Layout"
            onValueChange={(value: string[]) => {
              const [next] = value

              if (next === "down" || next === "across") {
                setDirection(next)
              }
            }}
            value={[direction]}
            variant="outline"
          >
            <ToggleGroupItem value="across">Left to right</ToggleGroupItem>
            <ToggleGroupItem value="down">Top down</ToggleGroupItem>
          </ToggleGroup>
        }
        subtitle="The Spinifex nodes, the switches they detect, and the speed and plane of every port that joins them. Hover a port or a node to trace its links."
        title="Topology"
      />
      {error && <ErrorBanner error={error} msg="Node facts did not load." />}
      <div className="h-[72vh] overflow-hidden rounded-lg border">
        {isPending ? (
          <p className="p-4 text-xs text-muted-foreground">
            Reading node facts…
          </p>
        ) : (
          <HoverContext value={hoverContext}>
            <ReactFlow
              colorMode={theme === "dark" ? "dark" : "light"}
              edgeTypes={EDGE_TYPES}
              edges={edges}
              fitView
              key={direction}
              nodeTypes={NODE_TYPES}
              nodes={nodes}
              nodesConnectable={false}
              onEdgeMouseEnter={(_, edge) => {
                setHover({ kind: "link", id: edge.id })
              }}
              onEdgeMouseLeave={() => {
                setHover(undefined)
              }}
              onNodeClick={(_, node) => {
                if (node.data.device.kind === "node") {
                  void navigate({
                    to: "/nodes",
                    search: {
                      selected: node.id.replace(/^node:/, ""),
                      tab: "network",
                    },
                  })
                }
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
        )}
      </div>
    </>
  )
}
