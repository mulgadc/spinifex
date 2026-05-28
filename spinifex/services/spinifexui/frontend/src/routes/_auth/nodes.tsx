import { useQuery } from "@tanstack/react-query"
import { createFileRoute, redirect } from "@tanstack/react-router"

import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { useAdmin } from "@/contexts/admin-context"
import {
  adminNodesQueryOptions,
  adminVMsQueryOptions,
  type NodeInfo,
  type VMInfo,
} from "@/queries/admin"

export const Route = createFileRoute("/_auth/nodes")({
  head: () => ({
    meta: [{ title: "Nodes | Mulga" }],
  }),
  component: NodesPage,
})

function formatUptime(seconds: number): string {
  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3600)
  if (days > 0) {
    return `${days}d ${hours}h`
  }
  const minutes = Math.floor((seconds % 3600) / 60)
  return hours > 0 ? `${hours}h ${minutes}m` : `${minutes}m`
}

function formatMemory(gb: number): string {
  if (gb >= 1) {
    return `${gb.toFixed(1)}Gi`
  }
  return `${Math.round(gb * 1024)}Mi`
}

function formatAge(launchTime: number): string {
  const seconds = Math.floor(Date.now() / 1000 - launchTime)
  if (seconds < 60) {
    return `${seconds}s`
  }
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) {
    return `${minutes}m`
  }
  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return `${hours}h ${minutes % 60}m`
  }
  const days = Math.floor(hours / 24)
  return `${days}d ${hours % 24}h`
}

function NodesPage() {
  const { isAdmin } = useAdmin()
  const { data: nodesData } = useQuery({
    ...adminNodesQueryOptions,
    enabled: isAdmin,
    refetchInterval: 5000,
  })
  const { data: vmsData } = useQuery({
    ...adminVMsQueryOptions,
    enabled: isAdmin,
    refetchInterval: 5000,
  })

  if (!isAdmin) {
    throw redirect({ to: "/" })
  }

  const nodes = nodesData?.nodes ?? []
  const vms = (vmsData?.vms ?? []).filter((vm) => !vm.managed_by)

  // Aggregate instance type capacity across all nodes
  const typeMap = new Map<
    string,
    { vcpu: number; memory_gb: number; available: number }
  >()
  for (const node of nodes) {
    for (const it of node.instance_types ?? []) {
      const existing = typeMap.get(it.name)
      if (existing) {
        existing.available += it.available
      } else {
        typeMap.set(it.name, {
          vcpu: it.vcpu,
          memory_gb: it.memory_gb,
          available: it.available,
        })
      }
    }
  }

  return (
    <>
      <PageHeading title="Nodes" />
      <div className="space-y-6">
        {/* Section 1: Nodes (spx get nodes) */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">Cluster Nodes</CardTitle>
          </CardHeader>
          <CardContent>
            {nodes.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                No nodes responding
              </p>
            ) : (
              <NodesTable nodes={nodes} />
            )}
          </CardContent>
        </Card>

        {/* Section 2: EC2 Instances */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">
              Elastic Compute Cloud (EC2)
            </CardTitle>
          </CardHeader>
          <CardContent>
            {vms.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                No instances running
              </p>
            ) : (
              <VMsTable vms={vms} />
            )}
          </CardContent>
        </Card>

        {/* Section 3: Resource Usage + Instance Types (spx top nodes) */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">Resource Usage</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            {nodes.length === 0 ? (
              <p className="text-xs text-muted-foreground">No data</p>
            ) : (
              <>
                <ResourceTable nodes={nodes} />
              </>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  )
}

function NodesTable({ nodes }: { nodes: NodeInfo[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="pr-4 pb-1 font-medium">Name</th>
            <th className="pr-4 pb-1 font-medium">Status</th>
            <th className="pr-4 pb-1 font-medium">Roles</th>
            <th className="pr-4 pb-1 font-medium">IP</th>
            <th className="pr-4 pb-1 font-medium">Region</th>
            <th className="pr-4 pb-1 font-medium">AZ</th>
            <th className="pr-4 pb-1 font-medium">Uptime</th>
            <th className="pr-4 pb-1 font-medium">EC2</th>
            <th className="pb-1 font-medium">Services</th>
          </tr>
        </thead>
        <tbody>
          {nodes.map((node) => {
            const roles = [
              node.nats_role ? `nats:${node.nats_role}` : null,
              node.predastore_role
                ? `predastore:${node.predastore_role}`
                : null,
            ]
              .filter(Boolean)
              .join(",")

            return (
              <tr key={node.node} className="border-b last:border-0">
                <td className="py-1.5 pr-4 font-mono font-medium">
                  {node.node}
                </td>
                <td className="py-1.5 pr-4">
                  <Badge
                    variant={node.status === "Ready" ? "default" : "secondary"}
                    className="text-[0.625rem]"
                  >
                    {node.status}
                  </Badge>
                </td>
                <td className="py-1.5 pr-4 font-mono text-muted-foreground">
                  {roles || "-"}
                </td>
                <td className="py-1.5 pr-4 font-mono">{node.host}</td>
                <td className="py-1.5 pr-4">{node.region}</td>
                <td className="py-1.5 pr-4">{node.az}</td>
                <td className="py-1.5 pr-4">{formatUptime(node.uptime)}</td>
                <td className="py-1.5 pr-4">{node.vm_count}</td>
                <td className="py-1.5">
                  <span className="text-muted-foreground">
                    {node.services?.join(",") ?? "-"}
                  </span>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function VMsTable({ vms }: { vms: VMInfo[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="pr-4 pb-1 font-medium">Instance</th>
            <th className="pr-4 pb-1 font-medium">Status</th>
            <th className="pr-4 pb-1 font-medium">Type</th>
            <th className="pr-4 pb-1 font-medium">vCPU</th>
            <th className="pr-4 pb-1 font-medium">Memory</th>
            <th className="pr-4 pb-1 font-medium">Node</th>
            <th className="pb-1 font-medium">Age</th>
          </tr>
        </thead>
        <tbody>
          {vms.map((vm) => (
            <tr key={vm.instance_id} className="border-b last:border-0">
              <td className="py-1.5 pr-4 font-mono">{vm.instance_id}</td>
              <td className="py-1.5 pr-4">
                <Badge
                  variant={vm.status === "running" ? "default" : "secondary"}
                  className="text-[0.625rem]"
                >
                  {vm.status}
                </Badge>
              </td>
              <td className="py-1.5 pr-4 font-mono">{vm.instance_type}</td>
              <td className="py-1.5 pr-4">{vm.vcpu}</td>
              <td className="py-1.5 pr-4">{formatMemory(vm.memory_gb)}</td>
              <td className="py-1.5 pr-4 font-mono">{vm.node}</td>
              <td className="py-1.5">
                {vm.launch_time > 0 ? formatAge(vm.launch_time) : "-"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ResourceTable({ nodes }: { nodes: NodeInfo[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="pr-4 pb-1 font-medium">Name</th>
            <th className="pr-4 pb-1 font-medium">CPU (used/total)</th>
            <th className="pr-4 pb-1 font-medium">MEM (used/total)</th>
            <th className="pb-1 font-medium">EC2</th>
          </tr>
        </thead>
        <tbody>
          {nodes.map((node) => (
            <tr key={node.node} className="border-b last:border-0">
              <td className="py-1.5 pr-4 font-mono font-medium">{node.node}</td>
              <td className="py-1.5 pr-4 font-mono">
                {node.alloc_vcpu}/{node.total_vcpu}
              </td>
              <td className="py-1.5 pr-4 font-mono">
                {formatMemory(node.alloc_mem_gb)}/
                {formatMemory(node.total_mem_gb)}
              </td>
              <td className="py-1.5">{node.vm_count}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
