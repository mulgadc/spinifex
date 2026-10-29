import type { Cluster } from "@aws-sdk/client-ecs"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { StateBadge } from "@/components/state-badge"
import { SystemImageRequired } from "@/components/system-image-required"
import { Button } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"

import { ClusterDetails } from "./cluster-details"
import { CreateClusterDialog } from "./create-cluster-dialog"
import { EcsSectionNav } from "./ecs-section-nav"

const COLUMNS: DataColumn<Cluster>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (cluster) => cluster.clusterName,
    cell: (cluster) => (
      <Link
        className="block max-w-72 min-w-0 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/ecs/list-clusters/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: cluster.clusterName ?? "",
        })}
        to="."
      >
        {cluster.clusterName}
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    sortValue: (cluster) => cluster.status,
    cell: (cluster) => <StateBadge state={cluster.status} />,
  },
  {
    id: "services",
    header: "Services",
    sortValue: (cluster) => cluster.activeServicesCount ?? 0,
    hideBelow: "sm",
    cell: (cluster) => cluster.activeServicesCount ?? 0,
  },
  {
    id: "tasks",
    header: "Running tasks",
    sortValue: (cluster) => cluster.runningTasksCount ?? 0,
    hideBelow: "md",
    cell: (cluster) => cluster.runningTasksCount ?? 0,
  },
  {
    id: "instances",
    header: "Instances",
    sortValue: (cluster) => cluster.registeredContainerInstancesCount ?? 0,
    hideBelow: "lg",
    cell: (cluster) => cluster.registeredContainerInstancesCount ?? 0,
  },
]

interface ClustersPageProps {
  clusters: readonly Cluster[]
  // Clusters launch their instances from the Spinifex-managed node image.
  hasSystemImage: boolean
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function ClustersPage({
  clusters,
  hasSystemImage,
  search,
  onSearchChange,
}: ClustersPageProps) {
  const [sort, setSort] = useState<SortState>()
  const [createOpen, setCreateOpen] = useState(false)

  const named = clusters.filter((cluster) => !!cluster.clusterName)

  const open = named.find(
    (cluster) =>
      search.selected !== "" && cluster.clusterName === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  let panel: ReactNode = null

  if (open) {
    panel = (
      <ClusterDetails
        cluster={open}
        key={open.clusterName}
        onClose={closePanel}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        tab={search.tab}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Button
            disabled={!hasSystemImage}
            onClick={() => {
              setCreateOpen(true)
            }}
          >
            <Plus data-icon="inline-start" />
            Create cluster
          </Button>
        }
        title="Clusters"
      />

      <EcsSectionNav />

      {!hasSystemImage && (
        <SystemImageRequired
          className="mb-6"
          description="ECS clusters need a Spinifex-managed node image (containerd + ecs-agent) to launch container instances. Import it before creating a cluster."
          importCommand="spx admin images import --name spinifex-ecs-node"
          title="ECS system image not found"
        />
      )}

      <DataTable
        activeId={open?.clusterName}
        caption="ECS clusters"
        columns={COLUMNS}
        empty="No ECS clusters found."
        getRowId={(cluster) => cluster.clusterName ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={named}
        sort={sort}
      />

      <CreateClusterDialog onOpenChange={setCreateOpen} open={createOpen} />
    </ResourceView>
  )
}
