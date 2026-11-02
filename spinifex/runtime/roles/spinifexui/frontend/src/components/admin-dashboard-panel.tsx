import { useQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { ChevronDown, ChevronRight, Lock, Network, Server } from "lucide-react"
import { type ReactNode, useState } from "react"

import type { SortState } from "@/components/data-table"
import { NodesTable } from "@/components/nodes-table"
import { StatusBadge } from "@/components/state-badge"
import { Badge } from "@/components/ui/badge"
import { buttonVariants } from "@/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"
import { useAdmin } from "@/hooks/use-admin"
import { CLOSED_PANEL } from "@/lib/panel-search"
import { adminNodesQueryOptions, adminVMsQueryOptions } from "@/queries/admin"
import { nodeFactsQueryOptions } from "@/queries/nodes"

const ADDONS = [
  { name: "Air-gap Support", key: "airgap" },
  { name: "NVIDIA Bluefield DPU", key: "bluefield" },
  { name: "NVIDIA MIG Support", key: "mig" },
] as const

function Stat({
  label,
  value,
  used,
  total,
}: {
  label: string
  value: ReactNode
  used?: number
  total?: number
}) {
  return (
    <div className="space-y-1.5 rounded-md border p-3">
      <div className="text-muted-foreground">{label}</div>
      <div className="text-base font-medium tabular-nums">{value}</div>
      {total !== undefined && used !== undefined && (
        <Progress aria-label={label} max={total || 1} value={used} />
      )}
    </div>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 truncate">{children}</span>
    </div>
  )
}

// The operator's view of the cluster: capacity at a glance, the nodes in the
// same table as the Nodes page, then the build and its licence.
export function AdminDashboardPanel() {
  const { isAdmin, version, license } = useAdmin()
  const navigate = useNavigate()
  const [nodesExpanded, setNodesExpanded] = useState(true)
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const { data: nodesData } = useQuery({
    ...adminNodesQueryOptions,
    enabled: isAdmin,
  })

  const { data: vmsData } = useQuery({
    ...adminVMsQueryOptions,
    enabled: isAdmin,
  })

  const { data: factsData } = useQuery({
    ...nodeFactsQueryOptions,
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
  const ready = nodes.filter((n) => n.status === "Ready").length

  const totalVCPU = nodes.reduce((sum, n) => sum + n.total_vcpu, 0)
  const allocVCPU = nodes.reduce((sum, n) => sum + n.alloc_vcpu, 0)
  const totalMem = nodes.reduce((sum, n) => sum + n.total_mem_gb, 0)
  const allocMem = nodes.reduce((sum, n) => sum + n.alloc_mem_gb, 0)

  return (
    <div className="mb-6 space-y-4">
      <Card>
        <CardHeader>
          <CardTitle variant="icon">
            <Server className="size-3.5" />
            Cluster
            <Badge variant="outline">{clusterMode}</Badge>
          </CardTitle>
          <CardAction>
            <Link
              className={buttonVariants({ variant: "outline", size: "sm" })}
              to="/nodes/topology"
            >
              <Network data-icon="inline-start" />
              Topology
            </Link>
          </CardAction>
        </CardHeader>
        <CardContent>
          <div className="space-y-4">
            <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
              <Stat label="Nodes ready" value={`${ready} / ${nodes.length}`} />
              <Stat label="Instances" value={vms.length} />
              <Stat
                label="vCPU allocated"
                total={totalVCPU}
                used={allocVCPU}
                value={`${allocVCPU} / ${totalVCPU}`}
              />
              <Stat
                label="Memory allocated"
                total={totalMem}
                used={allocMem}
                value={`${allocMem.toFixed(1)} / ${totalMem.toFixed(1)} GB`}
              />
            </div>

            {nodes.length > 0 && (
              <div className="space-y-2">
                <button
                  aria-expanded={nodesExpanded}
                  className="flex items-center gap-2 text-sm font-medium"
                  onClick={() => {
                    setNodesExpanded(!nodesExpanded)
                  }}
                  type="button"
                >
                  {nodesExpanded ? (
                    <ChevronDown className="size-3.5" />
                  ) : (
                    <ChevronRight className="size-3.5" />
                  )}
                  Nodes ({nodes.length})
                </button>
                {nodesExpanded && (
                  <NodesTable
                    facts={factsData?.nodes}
                    nodes={nodes}
                    onRowClick={(selected) => {
                      void navigate({
                        to: "/nodes",
                        search: { ...CLOSED_PANEL, selected },
                      })
                    }}
                    onSortChange={setSort}
                    sort={sort}
                  />
                )}
              </div>
            )}
          </div>
        </CardContent>
      </Card>

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Spinifex</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-1.5">
              <Row label="Version">
                <span className="font-mono">{version.version}</span>
              </Row>
              <Row label="Commit">
                <span className="font-mono">{version.commit}</span>
              </Row>
              <Row label="Platform">
                {version.os}/{version.arch}
              </Row>
              {region && <Row label="Region">{region}</Row>}
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>License &amp; Support</CardTitle>
            <CardAction>
              <Badge variant={isOpenSource ? "secondary" : "outline"}>
                {isOpenSource ? "Open Source (AGPLv3)" : "Commercial"}
              </Badge>
            </CardAction>
          </CardHeader>
          <CardContent>
            <div className="space-y-1.5">
              <Row label="Support">
                {isOpenSource ? "Community" : "Standard"}
              </Row>
              {ADDONS.map((addon) => (
                <Row key={addon.key} label={addon.name}>
                  {isOpenSource ? (
                    <Lock
                      aria-label="Commercial only"
                      className="size-3 text-muted-foreground"
                    />
                  ) : (
                    <StatusBadge tone="success">active</StatusBadge>
                  )}
                </Row>
              ))}
              {isOpenSource && (
                <a
                  className="block pt-1 text-xs font-medium text-primary hover:underline"
                  href="https://mulgadc.com/purchase"
                  rel="noopener noreferrer"
                  target="_blank"
                >
                  Upgrade to Commercial &rarr;
                </a>
              )}
            </div>
          </CardContent>
        </Card>
      </div>
    </div>
  )
}
