import { useQuery } from "@tanstack/react-query"
import {
  createFileRoute,
  Link,
  stripSearchParams,
} from "@tanstack/react-router"
import { Network } from "lucide-react"
import { type ReactNode, useState } from "react"
import { z } from "zod"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { GPUInventoryCard } from "@/components/gpu-inventory-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { StateBadge } from "@/components/state-badge"
import { Badge } from "@/components/ui/badge"
import { buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { formatLinkSpeed } from "@/lib/node-topology"
import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { requireAdmin } from "@/lib/require-admin"
import { formatVRAMMiB } from "@/lib/utils"
import {
  adminNodesQueryOptions,
  adminVMsQueryOptions,
  type InstanceTypeCap,
  type NodeInfo,
  type VMInfo,
} from "@/queries/admin"
import { type NodeFacts, nodeFactsQueryOptions } from "@/queries/nodes"

import { NodeDetails } from "./-components/node-details"

export const Route = createFileRoute("/_auth/nodes/")({
  validateSearch: z.object(panelSearchFields),
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  beforeLoad: requireAdmin,
  head: () => ({
    meta: [{ title: "Nodes | Mulga" }],
  }),
  component: NodesPage,
})

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

function formatUptime(seconds: number): string {
  const days = Math.floor(seconds / 86_400)
  const hours = Math.floor((seconds % 86_400) / 3600)

  if (days > 0) {
    return `${days}d ${hours}h`
  }

  const minutes = Math.floor((seconds % 3600) / 60)

  return hours > 0 ? `${hours}h ${minutes}m` : `${minutes}m`
}

interface ClusterNode {
  info: NodeInfo
  facts: NodeFacts | undefined
}

// The fastest port that is up, which is what the node's uplink can carry.
function fastestLink(facts: NodeFacts | undefined): number | undefined {
  const speeds = (facts?.nics ?? [])
    .filter((nic) => nic.state === "up" && nic.speed_mbps)
    .map((nic) => nic.speed_mbps ?? 0)

  return speeds.length > 0 ? Math.max(...speeds) : undefined
}

const NODE_COLUMNS: DataColumn<ClusterNode>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: ({ info }) => info.node,
    cell: ({ info }) => (
      <span className="font-mono text-xs font-medium text-primary">
        {info.node}
      </span>
    ),
  },
  {
    id: "status",
    header: "Status",
    sortValue: ({ info }) => info.status,
    cell: ({ info }) => (
      <Badge variant={info.status === "Ready" ? "default" : "secondary"}>
        {info.status}
      </Badge>
    ),
  },
  {
    id: "model",
    header: "Model",
    hideBelow: "md",
    sortValue: ({ facts }) => facts?.system.product,
    cell: ({ facts }) => (
      <span className="block max-w-56 truncate text-xs">
        {facts
          ? [facts.system.vendor, facts.system.product]
              .filter(Boolean)
              .join(" ") || "-"
          : "-"}
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
        {[
          info.nats_role && `nats:${info.nats_role}`,
          info.predastore_role && `predastore:${info.predastore_role}`,
        ]
          .filter(Boolean)
          .join(",") || "-"}
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

function NodesPage() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const { data: nodesData } = useQuery({
    ...adminNodesQueryOptions,
    refetchInterval: 5000,
  })

  const { data: vmsData } = useQuery({
    ...adminVMsQueryOptions,
    refetchInterval: 5000,
  })

  const { data: factsData } = useQuery(nodeFactsQueryOptions)

  const nodes = nodesData?.nodes ?? []
  const vms = (vmsData?.vms ?? []).filter((vm) => !vm.managed_by)

  const factsByNode = new Map(
    (factsData?.nodes ?? []).map((facts) => [facts.node, facts]),
  )

  const clusterNodes: ClusterNode[] = nodes.map((info) => ({
    info,
    facts: factsByNode.get(info.node),
  }))

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  const open = clusterNodes.find(
    ({ info }) => search.selected !== "" && info.node === search.selected,
  )

  let panel: ReactNode = null

  if (open) {
    panel = (
      <NodeDetails
        facts={open.facts}
        key={open.info.node}
        node={open.info}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        peers={nodes.map((node) => node.node)}
        tab={search.tab}
      />
    )
  }

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

  const instanceTypes = [...typeMap.entries()]
    .map(([name, data]) => ({ name, ...data }))
    .toSorted((a, b) => a.name.localeCompare(b.name))

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants({ variant: "outline" })}
            to="/nodes/topology"
          >
            <Network data-icon="inline-start" />
            Topology
          </Link>
        }
        title="Nodes"
      />
      <div className="space-y-6">
        <Card>
          <CardHeader>
            <CardTitle>Cluster Nodes</CardTitle>
          </CardHeader>
          <CardContent>
            <DataTable
              activeId={open?.info.node}
              caption="Cluster nodes"
              columns={NODE_COLUMNS}
              empty="No nodes responding."
              getRowId={({ info }) => info.node}
              onRowClick={(selected) => {
                onSearchChange({ ...CLOSED_PANEL, selected })
              }}
              onSortChange={setSort}
              rows={clusterNodes}
              sort={sort}
            />
          </CardContent>
        </Card>

        {/* Section 2: EC2 Instances */}
        <Card>
          <CardHeader>
            <CardTitle>Elastic Compute Cloud (EC2)</CardTitle>
          </CardHeader>
          <CardContent>
            <VMsTable vms={vms} />
          </CardContent>
        </Card>

        {/* Section 3: Resource Usage + Instance Types (spx top nodes) */}
        <Card>
          <CardHeader>
            <CardTitle>Resource Usage</CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-4">
              <ResourceTable nodes={nodes} />
              {instanceTypes.length > 0 && (
                <>
                  <h4 className="mt-4 text-xs font-medium text-muted-foreground">
                    Instance Type Capacity
                  </h4>
                  <InstanceTypesTable instanceTypes={instanceTypes} />
                </>
              )}
            </div>
          </CardContent>
        </Card>

        {/* Section 4: GPU Inventory */}
        {nodes.some((n) => (n.gpus?.length ?? 0) > 0) && (
          <Card>
            <CardHeader>
              <CardTitle>GPU Inventory</CardTitle>
            </CardHeader>
            <CardContent>
              <GPUInventoryCard nodes={nodes} />
            </CardContent>
          </Card>
        )}
      </div>
    </ResourceView>
  )
}

function formatVMGPUs(gpus: VMInfo["gpus"]): string {
  if (!gpus?.length) {
    return "-"
  }

  return gpus
    .map((gpu) =>
      gpu.profile
        ? `${gpu.profile} ${formatVRAMMiB(gpu.vram_mib)}`
        : `${gpu.model} ${formatVRAMMiB(gpu.vram_mib)}`,
    )
    .join(", ")
}

const VM_COLUMNS: DataColumn<VMInfo>[] = [
  {
    id: "instance",
    header: "Instance",
    sortValue: (vm) => vm.instance_id,
    cell: (vm) => <span className="font-mono text-xs">{vm.instance_id}</span>,
  },
  {
    id: "status",
    header: "Status",
    sortValue: (vm) => vm.status,
    cell: (vm) => <StateBadge state={vm.status} />,
  },
  {
    id: "type",
    header: "Type",
    sortValue: (vm) => vm.instance_type,
    cell: (vm) => <span className="font-mono text-xs">{vm.instance_type}</span>,
  },
  {
    id: "vcpu",
    header: "vCPU",
    sortValue: (vm) => vm.vcpu,
    align: "end",
    hideBelow: "md",
    cell: (vm) => <span className="tabular-nums">{vm.vcpu}</span>,
  },
  {
    id: "memory",
    header: "Memory",
    sortValue: (vm) => vm.memory_gb,
    align: "end",
    hideBelow: "md",
    cell: (vm) => (
      <span className="tabular-nums">{formatMemory(vm.memory_gb)}</span>
    ),
  },
  {
    id: "gpu",
    header: "GPU",
    hideBelow: "xl",
    cell: (vm) => (
      <span className="font-mono text-xs">{formatVMGPUs(vm.gpus)}</span>
    ),
  },
  {
    id: "node",
    header: "Node",
    sortValue: (vm) => vm.node,
    hideBelow: "sm",
    cell: (vm) => <span className="font-mono text-xs">{vm.node}</span>,
  },
  {
    id: "age",
    header: "Age",
    // An older launch sorts first ascending, so the column reads oldest-first.
    sortValue: (vm) => (vm.launch_time > 0 ? vm.launch_time : undefined),
    hideBelow: "lg",
    cell: (vm) => (
      <span className="tabular-nums">
        {vm.launch_time > 0 ? formatAge(vm.launch_time) : "-"}
      </span>
    ),
  },
]

function VMsTable({ vms }: { vms: VMInfo[] }) {
  const [sort, setSort] = useState<SortState>()

  return (
    <DataTable
      caption="EC2 instances"
      columns={VM_COLUMNS}
      empty="No instances running."
      getRowId={(vm) => vm.instance_id}
      onSortChange={setSort}
      rows={vms}
      sort={sort}
    />
  )
}

// Schedulable = host total minus the daemon reserve; this is the capacity a
// tenant deployment can actually consume.
function schedulableVcpu(node: NodeInfo): number {
  return node.total_vcpu - (node.reserved_vcpu ?? 0)
}

function schedulableMemGb(node: NodeInfo): number {
  return node.total_mem_gb - (node.reserved_mem_gb ?? 0)
}

const RESOURCE_COLUMNS: DataColumn<NodeInfo>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (node) => node.node,
    cell: (node) => (
      <span className="font-mono text-xs font-medium">{node.node}</span>
    ),
  },
  {
    id: "cpu",
    header: "CPU (used/sched)",
    sortValue: (node) => node.alloc_vcpu,
    align: "end",
    cell: (node) => (
      <span className="font-mono text-xs">
        {node.alloc_vcpu}/{schedulableVcpu(node)}
      </span>
    ),
  },
  {
    id: "memory",
    header: "MEM (used/sched)",
    sortValue: (node) => node.alloc_mem_gb,
    align: "end",
    cell: (node) => (
      <span className="font-mono text-xs">
        {formatMemory(node.alloc_mem_gb)}/{formatMemory(schedulableMemGb(node))}
      </span>
    ),
  },
  {
    id: "gpu",
    header: "GPU (used/total)",
    sortValue: (node) => (node.total_gpus > 0 ? node.alloc_gpus : undefined),
    align: "end",
    hideBelow: "md",
    cell: (node) => (
      <span className="font-mono text-xs">
        {node.total_gpus > 0 ? `${node.alloc_gpus}/${node.total_gpus}` : "-"}
      </span>
    ),
  },
  {
    id: "ec2",
    header: "EC2",
    sortValue: (node) => node.vm_count,
    align: "end",
    hideBelow: "sm",
    cell: (node) => <span className="tabular-nums">{node.vm_count}</span>,
  },
]

function ResourceTable({ nodes }: { nodes: NodeInfo[] }) {
  const [sort, setSort] = useState<SortState>()

  return (
    <DataTable
      caption="Resource usage by node"
      columns={RESOURCE_COLUMNS}
      empty="No nodes responding."
      getRowId={(node) => node.node}
      onSortChange={setSort}
      rows={nodes}
      sort={sort}
    />
  )
}

const INSTANCE_TYPE_COLUMNS: DataColumn<InstanceTypeCap>[] = [
  {
    id: "name",
    header: "Instance Type",
    sortValue: (it) => it.name,
    cell: (it) => <span className="font-mono text-xs">{it.name}</span>,
  },
  {
    id: "available",
    header: "Available",
    sortValue: (it) => it.available,
    align: "end",
    cell: (it) => <span className="tabular-nums">{it.available}</span>,
  },
  {
    id: "vcpu",
    header: "vCPU",
    sortValue: (it) => it.vcpu,
    align: "end",
    hideBelow: "sm",
    cell: (it) => <span className="tabular-nums">{it.vcpu}</span>,
  },
  {
    id: "memory",
    header: "Memory",
    sortValue: (it) => it.memory_gb,
    align: "end",
    hideBelow: "sm",
    cell: (it) => (
      <span className="tabular-nums">{formatMemory(it.memory_gb)}</span>
    ),
  },
]

function InstanceTypesTable({
  instanceTypes,
}: {
  instanceTypes: InstanceTypeCap[]
}) {
  const [sort, setSort] = useState<SortState>()

  return (
    <DataTable
      caption="Instance type capacity"
      columns={INSTANCE_TYPE_COLUMNS}
      empty="No instance types available."
      getRowId={(it) => it.name}
      onSortChange={setSort}
      rows={instanceTypes}
      sort={sort}
    />
  )
}
