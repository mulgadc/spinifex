import { useQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { StatusBadge } from "@/components/state-badge"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  adminStorageStatusQueryOptions,
  type BlobNode,
  type MetaNodeStatus,
} from "@/queries/admin"

export function ServiceMetricsPage() {
  const { data } = useQuery({
    ...adminStorageStatusQueryOptions,
    refetchInterval: 5000,
  })

  const encoding = data?.encoding
  const metaNodes = data?.meta_nodes ?? []
  const blobNodes = data?.blob_nodes ?? []

  const quorumHealthy = hasQuorum(metaNodes)

  return (
    <>
      <PageHeading title="Service Metrics" />
      <div className="space-y-6">
        {/* Section 1: Storage Overview */}
        <Card>
          <CardHeader>
            <CardTitle>Predastore (S3) Overview</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="grid grid-cols-2 gap-4 text-xs md:grid-cols-4">
              <div>
                <span className="text-muted-foreground">Encoding</span>
                <div className="mt-1">
                  {encoding ? (
                    <Badge variant="outline">
                      {encoding.type} RS({encoding.data_shards},
                      {encoding.parity_shards})
                    </Badge>
                  ) : (
                    <span className="text-muted-foreground">-</span>
                  )}
                </div>
              </div>
              <div>
                <span className="text-muted-foreground">Meta Nodes</span>
                <div className="mt-1 font-mono font-medium">
                  {metaNodes.length}
                </div>
              </div>
              <div>
                <span className="text-muted-foreground">Blob Nodes</span>
                <div className="mt-1 font-mono font-medium">
                  {blobNodes.length}
                </div>
              </div>
              <div>
                <span className="text-muted-foreground">Cluster Health</span>
                <div className="mt-1">
                  <ClusterHealthBadge
                    nodeCount={metaNodes.length}
                    quorumHealthy={quorumHealthy}
                  />
                </div>
              </div>
            </div>
            {encoding && (
              <p className="mt-3 text-2xs text-muted-foreground">
                Reed-Solomon erasure coding: {encoding.data_shards} data shards
                + {encoding.parity_shards} parity{" "}
                {encoding.parity_shards === 1 ? "shard" : "shards"}. Can
                tolerate {encoding.parity_shards} shard{" "}
                {encoding.parity_shards === 1 ? "failure" : "failures"} without
                data loss.
              </p>
            )}
          </CardContent>
        </Card>

        {/* Section 2: Meta Consensus Nodes */}
        <Card>
          <CardHeader>
            <CardTitle>Meta Consensus Nodes (Raft)</CardTitle>
          </CardHeader>
          <CardContent>
            <MetaNodesTable nodes={metaNodes} />
          </CardContent>
        </Card>

        {/* Section 3: Blob Nodes */}
        <Card>
          <CardHeader>
            <CardTitle>Object Storage Blob Nodes</CardTitle>
          </CardHeader>
          <CardContent>
            <BlobNodesTable nodes={blobNodes} />
          </CardContent>
        </Card>
      </div>
    </>
  )
}

// hasQuorum reads the raft view each meta node reports, since a leaderless
// replica still answers. A healthy node with no raft state was vouched for by
// its host's /readyz leader check, so it has no leader to compare.
function hasQuorum(nodes: MetaNodeStatus[]) {
  if (nodes.length === 0 || !nodes.every((n) => n.healthy)) {
    return false
  }

  const leaders = new Set(nodes.flatMap((n) => (n.state ? [n.leader] : [])))

  return leaders.size <= 1 && !leaders.has("") && !leaders.has(undefined)
}

function ClusterHealthBadge({
  nodeCount,
  quorumHealthy,
}: {
  nodeCount: number
  quorumHealthy: boolean
}) {
  if (nodeCount === 0) {
    return <StatusBadge tone="neutral">Unknown</StatusBadge>
  }

  if (quorumHealthy) {
    return <StatusBadge tone="success">Healthy</StatusBadge>
  }

  return <StatusBadge tone="danger">Degraded</StatusBadge>
}

const DASH = <span className="text-muted-foreground">-</span>

const META_COLUMNS: DataColumn<MetaNodeStatus>[] = [
  {
    id: "node",
    header: "Node",
    sortValue: (node) => node.id,
    cell: (node) => (
      <span className="font-mono text-xs font-medium">{node.id}</span>
    ),
  },
  {
    id: "host",
    header: "Host",
    sortValue: (node) => node.host,
    cell: (node) => <span className="font-mono text-xs">{node.host}</span>,
  },
  {
    id: "port",
    header: "Port",
    sortValue: (node) => node.port,
    hideBelow: "lg",
    cell: (node) => <span className="font-mono text-xs">{node.port}</span>,
  },
  {
    id: "health",
    header: "Health",
    sortValue: (node) => (node.healthy ? 0 : 1),
    cell: (node) => (
      <StatusBadge tone={node.healthy ? "success" : "danger"}>
        {node.healthy ? "Healthy" : "Unreachable"}
      </StatusBadge>
    ),
  },
  {
    id: "state",
    header: "State",
    sortValue: (node) => node.state || undefined,
    cell: (node) =>
      node.state ? (
        <Badge variant={node.state === "Leader" ? "outline" : "secondary"}>
          {node.state}
        </Badge>
      ) : (
        DASH
      ),
  },
  {
    id: "leader",
    header: "Leader",
    sortValue: (node) => node.leader || undefined,
    hideBelow: "sm",
    cell: (node) => (
      <span className="font-mono text-xs">{node.leader ?? DASH}</span>
    ),
  },
  {
    id: "term",
    header: "Term",
    sortValue: (node) => node.term,
    align: "end",
    hideBelow: "md",
    cell: (node) => (
      <span className="font-mono text-xs">{node.term ?? DASH}</span>
    ),
  },
  {
    id: "commit",
    header: "Commit Index",
    sortValue: (node) => node.commit_index,
    align: "end",
    hideBelow: "xl",
    cell: (node) => (
      <span className="font-mono text-xs">{node.commit_index ?? DASH}</span>
    ),
  },
  {
    id: "applied",
    header: "Applied Index",
    sortValue: (node) => node.applied_index,
    align: "end",
    hideBelow: "xl",
    cell: (node) => (
      <span className="font-mono text-xs">{node.applied_index ?? DASH}</span>
    ),
  },
]

function MetaNodesTable({ nodes }: { nodes: MetaNodeStatus[] }) {
  const [sort, setSort] = useState<SortState>()

  return (
    <DataTable
      caption="Meta consensus nodes"
      columns={META_COLUMNS}
      empty="No meta nodes found."
      getRowId={(node) => String(node.id)}
      onSortChange={setSort}
      rows={nodes}
      sort={sort}
    />
  )
}

const BLOB_COLUMNS: DataColumn<BlobNode>[] = [
  {
    id: "node",
    header: "Node",
    sortValue: (node) => node.id,
    cell: (node) => (
      <span className="font-mono text-xs font-medium">{node.id}</span>
    ),
  },
  {
    id: "host",
    header: "Host",
    sortValue: (node) => node.host,
    cell: (node) => <span className="font-mono text-xs">{node.host}</span>,
  },
  {
    id: "port",
    header: "Port",
    sortValue: (node) => node.port,
    cell: (node) => <span className="font-mono text-xs">{node.port}</span>,
  },
]

function BlobNodesTable({ nodes }: { nodes: BlobNode[] }) {
  const [sort, setSort] = useState<SortState>()

  return (
    <DataTable
      caption="Blob nodes"
      columns={BLOB_COLUMNS}
      empty="No blob nodes found."
      getRowId={(node) => String(node.id)}
      onSortChange={setSort}
      rows={nodes}
      sort={sort}
    />
  )
}
