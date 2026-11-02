import type { Cluster } from "@aws-sdk/client-ecs"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
import { CreatePanel } from "@/components/create-panel"
import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { SelectionBar } from "@/components/selection-bar"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { useDeleteCluster } from "@/mutations/ecs"

import { ClusterDetails } from "./cluster-details"
import { CLOSED_CLUSTERS_PANEL, type ClustersSearch } from "./clusters-search"
import { CreateClusterPage } from "./create-cluster-page"
import { CreateServicePage } from "./create-service-page"
import { EcsSectionNav } from "./ecs-section-nav"
import { RunTaskPage } from "./run-task-page"

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
          ...CLOSED_CLUSTERS_PANEL,
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
  search: ClustersSearch
  onSearchChange: (search: Partial<ClustersSearch>) => void
}

export function ClustersPage({
  clusters,
  search,
  onSearchChange,
}: ClustersPageProps) {
  const [sort, setSort] = useState<SortState>()

  const named = clusters.filter((cluster) => !!cluster.clusterName)

  const open = named.find(
    (cluster) =>
      search.selected !== "" && cluster.clusterName === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_CLUSTERS_PANEL)
  }

  // A launch closes back onto the cluster it was opened from.
  function closeLaunch() {
    onSearchChange({ launch: "" })
  }

  const selection = useRowSelection(
    named.map((cluster) => cluster.clusterName ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteCluster = useDeleteCluster()

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel onClose={closePanel} title="Create cluster">
        <CreateClusterPage
          onClose={(clusterName) => {
            onSearchChange({
              ...CLOSED_CLUSTERS_PANEL,
              selected: clusterName ?? "",
            })
          }}
        />
      </CreatePanel>
    )
  } else if (open && search.launch === "service") {
    panel = (
      <CreatePanel onClose={closeLaunch} title="Create service">
        <CreateServicePage cluster={search.selected} onClose={closeLaunch} />
      </CreatePanel>
    )
  } else if (open && search.launch === "task") {
    panel = (
      <CreatePanel onClose={closeLaunch} title="Run task">
        <RunTaskPage cluster={search.selected} onClose={closeLaunch} />
      </CreatePanel>
    )
  } else if (open) {
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
          <Link
            className={buttonVariants()}
            from="/ecs/list-clusters/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_CLUSTERS_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create cluster
          </Link>
        }
        title="Clusters"
      />

      <EcsSectionNav />

      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            confirmPhrase="delete"
            deleteOne={deleteCluster.mutateAsync}
            description="This permanently deletes these clusters and force-stops every task they hold. This cannot be undone."
            ids={selection.ids}
            noun={["cluster", "clusters"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}

      <DataTable
        activeId={open?.clusterName}
        caption="ECS clusters"
        columns={COLUMNS}
        empty="No ECS clusters found."
        getRowId={(cluster) => cluster.clusterName ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_CLUSTERS_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={named}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
