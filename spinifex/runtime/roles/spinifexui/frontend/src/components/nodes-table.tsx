import { Link } from "@tanstack/react-router"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { StateBadge } from "@/components/state-badge"
import { formatLinkSpeed } from "@/lib/node-topology"
import { CLOSED_PANEL } from "@/lib/panel-search"
import type { NodeInfo } from "@/queries/admin"
import type { NodeFacts } from "@/queries/nodes"

interface ClusterNode {
  info: NodeInfo
  facts: NodeFacts | undefined
}

function formatUptime(seconds: number): string {
  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3600)

  if (days > 0) {
    return `${days}d ${hours}h`
  }

  const minutes = Math.floor((seconds % 3600) / 60)

  return hours > 0 ? `${hours}h ${minutes}m` : `${minutes}m`
}

// The fastest port that is up, which is what the node's uplink can carry.
function fastestLink(facts: NodeFacts | undefined): number | undefined {
  const speeds = (facts?.nics ?? [])
    .filter((nic) => nic.state === "up" && nic.speed_mbps)
    .map((nic) => nic.speed_mbps ?? 0)

  return speeds.length > 0 ? Math.max(...speeds) : undefined
}

function roles(info: NodeInfo): string {
  return (
    [
      info.nats_role && `nats:${info.nats_role}`,
      info.predastore_role && `predastore:${info.predastore_role}`,
    ]
      .filter(Boolean)
      .join(",") || "-"
  )
}

const NODE_COLUMNS: DataColumn<ClusterNode>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: ({ info }) => info.node,
    cell: ({ info }) => (
      <Link
        className="rounded-sm font-mono text-xs font-medium whitespace-nowrap text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        search={{ ...CLOSED_PANEL, selected: info.node }}
        to="/nodes"
      >
        {info.node}
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    sortValue: ({ info }) => info.status,
    cell: ({ info }) => <StateBadge state={info.status} />,
  },
  {
    id: "model",
    header: "Model",
    hideBelow: "md",
    sortValue: ({ facts }) => facts?.system.product,
    cell: ({ facts }) => (
      <span className="block max-w-56 truncate text-xs">
        {[facts?.system.vendor, facts?.system.product]
          .filter(Boolean)
          .join(" ") || "-"}
      </span>
    ),
  },
  {
    id: "cpu",
    header: "CPU",
    hideBelow: "lg",
    sortValue: ({ facts }) => facts?.cpu.threads,
    cell: ({ facts }) => (
      <span className="text-xs tabular-nums">
        {facts ? `${facts.cpu.threads} threads` : "-"}
      </span>
    ),
  },
  {
    id: "link",
    header: "Fastest port",
    hideBelow: "lg",
    sortValue: ({ facts }) => fastestLink(facts),
    cell: ({ facts }) => (
      <span className="text-xs tabular-nums">
        {formatLinkSpeed(fastestLink(facts))}
      </span>
    ),
  },
  {
    id: "roles",
    header: "Roles",
    hideBelow: "xl",
    cell: ({ info }) => (
      <span className="font-mono text-xs text-muted-foreground">
        {roles(info)}
      </span>
    ),
  },
  {
    id: "ip",
    header: "IP",
    hideBelow: "sm",
    sortValue: ({ info }) => info.host,
    cell: ({ info }) => <span className="font-mono text-xs">{info.host}</span>,
  },
  {
    id: "az",
    header: "AZ",
    hideBelow: "xl",
    sortValue: ({ info }) => info.az,
    cell: ({ info }) => <span className="text-xs">{info.az}</span>,
  },
  {
    id: "uptime",
    header: "Uptime",
    hideBelow: "md",
    sortValue: ({ info }) => info.uptime,
    cell: ({ info }) => (
      <span className="tabular-nums">{formatUptime(info.uptime)}</span>
    ),
  },
  {
    id: "ec2",
    header: "EC2",
    align: "end",
    sortValue: ({ info }) => info.vm_count,
    cell: ({ info }) => <span className="tabular-nums">{info.vm_count}</span>,
  },
]

const NO_FACTS: readonly NodeFacts[] = []

// The cluster's nodes as `spx get nodes` lists them, beside the hardware each
// reports. The name links to the node on the Nodes page.
export function NodesTable({
  nodes,
  facts = NO_FACTS,
  activeId,
  onRowClick,
  sort,
  onSortChange,
}: {
  nodes: readonly NodeInfo[]
  facts?: readonly NodeFacts[]
  activeId?: string
  onRowClick: (node: string) => void
  sort: SortState
  onSortChange: (sort: SortState) => void
}) {
  const byNode = new Map(facts.map((f) => [f.node, f]))

  const rows: ClusterNode[] = nodes.map((info) => ({
    info,
    facts: byNode.get(info.node),
  }))

  return (
    <DataTable
      activeId={activeId}
      caption="Cluster nodes"
      columns={NODE_COLUMNS}
      empty="No nodes responding."
      getRowId={({ info }) => info.node}
      onRowClick={onRowClick}
      onSortChange={onSortChange}
      rows={rows}
      sort={sort}
    />
  )
}
