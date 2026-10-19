import { useQuery } from "@tanstack/react-query"
import { ChevronDown, ChevronRight, Lock, Monitor, Server } from "lucide-react"
import { useState } from "react"

import { NodesTable } from "@/components/nodes-table"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { useAdmin } from "@/hooks/use-admin"
import { adminNodesQueryOptions, adminVMsQueryOptions } from "@/queries/admin"

const ADDONS = [
  { name: "Air-gap Support", key: "airgap" },
  { name: "NVIDIA Bluefield DPU", key: "bluefield" },
  { name: "NVIDIA MIG Support", key: "mig" },
] as const

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
          <CardHeader className="mb-2">
            <CardTitle variant="icon">
              <Monitor className="size-3.5" />
              Spinifex Info
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-1 text-xs">
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
            </div>
          </CardContent>
        </Card>

        {/* License & Support */}
        <Card>
          <CardHeader className="mb-2">
            <CardTitle>License &amp; Support</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-2 text-xs">
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
            </div>
          </CardContent>
        </Card>

        {/* Cluster Status */}
        <Card>
          <CardHeader className="mb-2">
            <CardTitle variant="icon">
              <Server className="size-3.5" />
              Cluster Status
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-2 text-xs">
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
            </div>
          </CardContent>
        </Card>
      </div>

      {/* Collapsible Nodes Table */}
      {nodes.length > 0 && (
        <Card>
          <CardHeader>
            <button
              type="button"
              className="flex items-center gap-2 text-sm font-medium"
              onClick={() => {
                setNodesExpanded(!nodesExpanded)
              }}
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
            <CardContent className="mt-2">
              <NodesTable compact={true} nodes={nodes} />
            </CardContent>
          )}
        </Card>
      )}

      {/* Add-ons Matrix */}
      <Card>
        <CardHeader className="mb-2">
          <CardTitle>Commercial Add-ons</CardTitle>
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
                  <Badge variant="outline">Active</Badge>
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
