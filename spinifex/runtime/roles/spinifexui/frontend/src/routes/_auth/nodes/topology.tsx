import "@xyflow/react/dist/style.css"
import { useQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import {
  Background,
  Controls,
  type Edge,
  Handle,
  type Node,
  type NodeProps,
  Position,
  ReactFlow,
} from "@xyflow/react"
import { useMemo } from "react"

import { DeviceArt } from "@/components/device-art"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { useTheme } from "@/components/theme-provider"
import {
  buildTopology,
  formatLinkSpeed,
  layoutTopology,
  planesLabel,
  type TopologyDevice,
} from "@/lib/node-topology"
import { requireAdmin } from "@/lib/require-admin"
import { cn } from "@/lib/utils"
import { nodeFactsQueryOptions } from "@/queries/nodes"

export const Route = createFileRoute("/_auth/nodes/topology")({
  beforeLoad: requireAdmin,
  head: () => ({
    meta: [{ title: "Topology | Nodes | Mulga" }],
  }),
  component: TopologyPage,
})

type DeviceNode = Node<{ device: TopologyDevice }, "device">

function DeviceCard({ data }: NodeProps<DeviceNode>) {
  const { device } = data

  return (
    <div
      className={cn(
        "flex w-44 flex-col items-center gap-1 rounded-lg border bg-card p-3 text-card-foreground shadow-sm",
        device.kind === "node" && "cursor-pointer hover:border-primary",
      )}
    >
      <Handle
        className="opacity-0"
        isConnectable={false}
        position={Position.Top}
        type="target"
      />
      <DeviceArt
        className="h-12 w-16 text-muted-foreground"
        formFactor={device.formFactor}
        kind={device.kind}
      />
      <span className="max-w-full truncate font-mono text-xs font-medium">
        {device.label}
      </span>
      {device.detail && (
        <span className="max-w-full truncate text-2xs text-muted-foreground">
          {device.detail}
        </span>
      )}
      {device.planes.length > 0 && (
        <span className="text-2xs text-muted-foreground">
          {planesLabel(device.planes)}
        </span>
      )}
      <Handle
        className="opacity-0"
        isConnectable={false}
        position={Position.Bottom}
        type="source"
      />
    </div>
  )
}

const NODE_TYPES = { device: DeviceCard }

// Edges leave a card's bottom and enter the next tier's top, so each link is
// drawn from whichever end sits higher in the layout.
function TopologyPage() {
  const { data, error, isPending } = useQuery(nodeFactsQueryOptions)
  const { theme } = useTheme()
  const navigate = useNavigate()

  const { nodes, edges } = useMemo(() => {
    const topology = buildTopology(data?.nodes ?? [])

    const placed = new Map(
      layoutTopology(topology).map((place) => [place.id, place]),
    )

    const flowNodes: DeviceNode[] = topology.devices.map((device) => ({
      id: device.id,
      type: "device",
      position: {
        x: placed.get(device.id)?.x ?? 0,
        y: placed.get(device.id)?.y ?? 0,
      },
      data: { device },
      draggable: true,
      connectable: false,
    }))

    const flowEdges: Edge[] = topology.links.map((link) => {
      const sourceY = placed.get(link.source)?.y ?? 0
      const targetY = placed.get(link.target)?.y ?? 0

      const [upper, lower] =
        sourceY <= targetY
          ? [link.source, link.target]
          : [link.target, link.source]

      return {
        id: link.id,
        source: upper,
        target: lower,
        label: [
          link.nic && link.port ? `${link.nic} ↔ ${link.port}` : link.nic,
          link.speedMbps ? formatLinkSpeed(link.speedMbps) : undefined,
        ]
          .filter(Boolean)
          .join(" · "),
        labelStyle: { fontSize: 10 },
        style: { strokeWidth: link.planes.includes("wan") ? 2.5 : 1.5 },
      }
    })

    return { nodes: flowNodes, edges: flowEdges }
  }, [data])

  return (
    <>
      <PageHeading
        subtitle="Switch ports come from LLDP where a node's switch sends it, and are grouped by plane where it does not."
        title="Topology"
      />
      {error && <ErrorBanner error={error} msg="Node facts did not load." />}
      <div className="h-[70vh] overflow-hidden rounded-lg border">
        {isPending ? (
          <p className="p-4 text-xs text-muted-foreground">
            Reading node facts…
          </p>
        ) : (
          <ReactFlow
            colorMode={theme === "dark" ? "dark" : "light"}
            edges={edges}
            fitView
            nodeTypes={NODE_TYPES}
            nodes={nodes}
            nodesConnectable={false}
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
          >
            <Background />
            <Controls showInteractive={false} />
          </ReactFlow>
        )}
      </div>
    </>
  )
}
