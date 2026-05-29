import { useQuery } from "@tanstack/react-query"
import { ChevronDown, ChevronRight, Lock, Monitor, Server } from "lucide-react"
import { useState } from "react"

import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { useAdmin } from "@/contexts/admin-context"
import {
  adminNodesQueryOptions,
  adminVMsQueryOptions,
  type NodeInfo,
} from "@/queries/admin"

const ADDONS = [
  { name: "Air-gap Support", key: "airgap" },
  { name: "NVIDIA Bluefield DPU", key: "bluefield" },
  { name: "NVIDIA MIG Support", key: "mig" },
] as const

function formatUptime(seconds: number): string {
  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3600)
  if (days > 0) {
    return `${days}d ${hours}h`
  }
  const minutes = Math.floor((seconds % 3600) / 60)
  return hours > 0 ? `${hours}h ${minutes}m` : `${minutes}m`
}

export function AdminDashboardPanel() {
  const { isAdmin, version, license } = useAdmin()
  const [nodesExpanded, setNodesExpanded] = useState(false)

  const { data: nodesData } = useQuery({
    ...adminNodesQueryOptions,
    enabled: isAdmin,
  })
  const { data: vmsData } = useQuery({
    ...adminVMsQueryOptions,
    enabled: isAdmin,
  })

  if (!isAdmin || !version) {
    return null
  }

  const nodes = nodesData?.nodes ?? []
  const vms = vmsData?.vms ?? []
  const clusterMode = nodesData?.cluster_mode ?? "single-node"
  const isOpenSource = license === "open-source"
  const region = nodes[0]?.region ?? null

  const totalVCPU = nodes.reduce((sum, n) => sum + n.total_vcpu, 0)
  const allocVCPU = nodes.reduce((sum, n) => sum + n.alloc_vcpu, 0)
  const totalMem = nodes.reduce((sum, n) => sum + n.total_mem_gb, 0)
  const allocMem = nodes.reduce((sum, n) => sum + n.alloc_mem_gb, 0)

  return (
    <div className="mb-6 space-y-4">
      <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
        {/* Version Info */}
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <Monitor className="size-3.5" />
              Spinifex Info
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-1 text-xs">
            <div className="flex justify-between">
              <span className="text-muted-foreground">Version</span>
              <span className="font-mono">{version.version}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">Commit</span>
              <span className="font-mono">{version.commit}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">Platform</span>
              <span>
                {version.os}/{version.arch}
              </span>
            </div>
            {region && (
              <div className="flex justify-between">
                <span className="text-muted-foreground">Region</span>
                <span>{region}</span>
              </div>
            )}
          </CardContent>
        </Card>

        {/* License & Support */}
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">License &amp; Support</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2 text-xs">
            <div className="flex items-center justify-between">
              <span className="text-muted-foreground">License</span>
              <Badge variant={isOpenSource ? "secondary" : "default"}>
                {isOpenSource ? "Open Source (AGPLv3)" : "Commercial"}
              </Badge>
            </div>
            <div className="flex items-center justify-between">
              <span className="text-muted-foreground">Support</span>
              <span>{isOpenSource ? "Community" : "Standard"}</span>
            </div>
            {isOpenSource && (
              <a
                href="https://mulgadc.com/purchase"
                target="_blank"
                rel="noopener noreferrer"
                className="mt-2 block text-xs font-medium text-primary hover:underline"
              >
                Upgrade to Commercial &rarr;
              </a>
            )}
          </CardContent>
        </Card>

        {/* Cluster Status */}
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <Server className="size-3.5" />
              Cluster Status
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-2 text-xs">
            <div className="flex items-center gap-2">
              <Badge variant="outline">{clusterMode}</Badge>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">Nodes</span>
              <span>{nodes.length}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">Instances</span>
              <span>{vms.length}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">vCPU</span>
              <span>
                {allocVCPU} / {totalVCPU}
              </span>
            </div>
            <div className="flex justify-between">
              <span className="text-muted-foreground">Memory</span>
              <span>
                {allocMem.toFixed(1)} / {totalMem.toFixed(1)} GB
              </span>
            </div>
          </CardContent>
        </Card>
      </div>

      {/* Collapsible Nodes Table */}
      {nodes.length > 0 && (
        <Card>
          <CardHeader className="pb-0">
            <button
              type="button"
              className="flex items-center gap-2 text-sm font-medium"
              onClick={() => setNodesExpanded(!nodesExpanded)}
            >
              {nodesExpanded ? (
                <ChevronDown className="size-3.5" />
              ) : (
                <ChevronRight className="size-3.5" />
              )}
              Nodes ({nodes.length})
            </button>
          </CardHeader>
          {nodesExpanded && (
            <CardContent className="pt-2">
              <NodesTable nodes={nodes} />
            </CardContent>
          )}
        </Card>
      )}

      {/* Add-ons Matrix */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm">Commercial Add-ons</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="space-y-2">
            {ADDONS.map((addon) => (
              <div
                key={addon.key}
                className="flex items-center justify-between text-xs"
              >
                <span
                  className={isOpenSource ? "text-muted-foreground" : undefined}
                >
                  {addon.name}
                </span>
                {isOpenSource ? (
                  <Lock className="size-3 text-muted-foreground" />
                ) : (
                  <Badge variant="outline" className="text-[0.625rem]">
                    Active
                  </Badge>
                )}
              </div>
            ))}
          </div>
          {isOpenSource && (
            <a
              href="https://mulgadc.com/purchase"
              target="_blank"
              rel="noopener noreferrer"
              className="mt-3 block text-xs font-medium text-primary hover:underline"
            >
              Learn more about commercial features &rarr;
            </a>
          )}
        </CardContent>
      </Card>
    </div>
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
          {nodes.map((node) => (
            <tr key={node.node} className="border-b last:border-0">
              <td className="py-1.5 pr-4 font-mono font-medium">{node.node}</td>
              <td className="py-1.5 pr-4">
                <Badge
                  variant={node.status === "Ready" ? "default" : "secondary"}
                  className="text-[0.625rem]"
                >
                  {node.status}
                </Badge>
              </td>
              <td className="py-1.5 pr-4">
                <div className="flex gap-1">
                  {node.nats_role && (
                    <span className="text-muted-foreground">
                      nats:{node.nats_role}
                    </span>
                  )}
                  {node.predastore_role && (
                    <span className="text-muted-foreground">
                      ps:{node.predastore_role}
                    </span>
                  )}
                </div>
              </td>
              <td className="py-1.5 pr-4 font-mono">{node.host}</td>
              <td className="py-1.5 pr-4">{node.region}</td>
              <td className="py-1.5 pr-4">{node.az}</td>
              <td className="py-1.5 pr-4">{formatUptime(node.uptime)}</td>
              <td className="py-1.5 pr-4">{node.vm_count}</td>
              <td className="py-1.5">
                <span className="text-muted-foreground">
                  {node.services.length}
                </span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
