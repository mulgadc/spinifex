import { useQuery } from "@tanstack/react-query"
import { createFileRoute, redirect } from "@tanstack/react-router"

import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { useAdmin } from "@/contexts/admin-context"
import {
  adminStorageStatusQueryOptions,
  type DBNodeStatus,
  type ShardNode,
} from "@/queries/admin"

export const Route = createFileRoute("/_auth/s3/service-metrics")({
  head: () => ({
    meta: [{ title: "Service Metrics | S3 | Mulga" }],
  }),
  component: ServiceMetricsPage,
})

function ServiceMetricsPage() {
  const { isAdmin } = useAdmin()
  const { data } = useQuery({
    ...adminStorageStatusQueryOptions,
    enabled: isAdmin,
    refetchInterval: 5000,
  })

  if (!isAdmin) {
    throw redirect({ to: "/" })
  }

  const encoding = data?.encoding
  const dbNodes = data?.db_nodes ?? []
  const shardNodes = data?.shard_nodes ?? []

  const allHealthy = dbNodes.length > 0 && dbNodes.every((n) => n.healthy)
  const anyUnhealthy = dbNodes.some((n) => !n.healthy)

  return (
    <>
      <PageHeading title="Service Metrics" />
      <div className="space-y-6">
        {/* Section 1: Storage Overview */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">Predastore (S3) Overview</CardTitle>
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
                <span className="text-muted-foreground">DB Nodes</span>
                <div className="mt-1 font-mono font-medium">
                  {dbNodes.length}
                </div>
              </div>
              <div>
                <span className="text-muted-foreground">Shard Nodes</span>
                <div className="mt-1 font-mono font-medium">
                  {shardNodes.length}
                </div>
              </div>
              <div>
                <span className="text-muted-foreground">Cluster Health</span>
                <div className="mt-1">
                  <ClusterHealthBadge
                    nodeCount={dbNodes.length}
                    allHealthy={allHealthy}
                    anyUnhealthy={anyUnhealthy}
                  />
                </div>
              </div>
            </div>
            {encoding && (
              <p className="mt-3 text-[0.625rem] text-muted-foreground">
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

        {/* Section 2: DB Consensus Nodes */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">DB Consensus Nodes (Raft)</CardTitle>
          </CardHeader>
          <CardContent>
            {dbNodes.length === 0 ? (
              <p className="text-xs text-muted-foreground">No data</p>
            ) : (
              <DBNodesTable nodes={dbNodes} />
            )}
          </CardContent>
        </Card>

        {/* Section 3: Shard Nodes */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">
              Object Storage Shard Nodes
            </CardTitle>
          </CardHeader>
          <CardContent>
            {shardNodes.length === 0 ? (
              <p className="text-xs text-muted-foreground">No data</p>
            ) : (
              <ShardNodesTable nodes={shardNodes} />
            )}
          </CardContent>
        </Card>
      </div>
    </>
  )
}

function ClusterHealthBadge({
  nodeCount,
  allHealthy,
  anyUnhealthy,
}: {
  nodeCount: number
  allHealthy: boolean
  anyUnhealthy: boolean
}) {
  if (nodeCount === 0) {
    return <Badge variant="secondary">Unknown</Badge>
  }
  if (allHealthy) {
    return <Badge variant="default">Healthy</Badge>
  }
  if (anyUnhealthy) {
    return <Badge variant="destructive">Degraded</Badge>
  }
  return <Badge variant="secondary">Unknown</Badge>
}

function DBNodesTable({ nodes }: { nodes: DBNodeStatus[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="pr-4 pb-1 font-medium">Node</th>
            <th className="pr-4 pb-1 font-medium">Host</th>
            <th className="pr-4 pb-1 font-medium">Port</th>
            <th className="pr-4 pb-1 font-medium">Health</th>
            <th className="pr-4 pb-1 font-medium">State</th>
            <th className="pr-4 pb-1 font-medium">Leader</th>
            <th className="pr-4 pb-1 font-medium">Term</th>
            <th className="pr-4 pb-1 font-medium">Commit Index</th>
            <th className="pb-1 font-medium">Applied Index</th>
          </tr>
        </thead>
        <tbody>
          {nodes.map((node) => (
            <tr key={node.id} className="border-b last:border-0">
              <td className="py-1.5 pr-4 font-mono font-medium">{node.id}</td>
              <td className="py-1.5 pr-4 font-mono">{node.host}</td>
              <td className="py-1.5 pr-4 font-mono">{node.port}</td>
              <td className="py-1.5 pr-4">
                <Badge
                  variant={node.healthy ? "default" : "destructive"}
                  className="text-[0.625rem]"
                >
                  {node.healthy ? "Healthy" : "Unreachable"}
                </Badge>
              </td>
              <td className="py-1.5 pr-4">
                {node.state ? (
                  <Badge
                    variant={node.state === "Leader" ? "default" : "secondary"}
                    className="text-[0.625rem]"
                  >
                    {node.state}
                  </Badge>
                ) : (
                  <span className="text-muted-foreground">-</span>
                )}
              </td>
              <td className="py-1.5 pr-4 font-mono">{node.leader ?? "-"}</td>
              <td className="py-1.5 pr-4 font-mono">{node.term ?? "-"}</td>
              <td className="py-1.5 pr-4 font-mono">
                {node.commit_index ?? "-"}
              </td>
              <td className="py-1.5 font-mono">{node.applied_index ?? "-"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ShardNodesTable({ nodes }: { nodes: ShardNode[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="pr-4 pb-1 font-medium">Node</th>
            <th className="pr-4 pb-1 font-medium">Host</th>
            <th className="pb-1 font-medium">Port</th>
          </tr>
        </thead>
        <tbody>
          {nodes.map((node) => (
            <tr key={node.id} className="border-b last:border-0">
              <td className="py-1.5 pr-4 font-mono font-medium">{node.id}</td>
              <td className="py-1.5 pr-4 font-mono">{node.host}</td>
              <td className="py-1.5 font-mono">{node.port}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
