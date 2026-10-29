import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, useMemo, useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { eksClusterQueryOptions } from "@/queries/eks"

import { ClusterDetails } from "./cluster-details"

// ListClusters returns names only, so each row describes its own cluster.
function ClusterStatus({ clusterName }: { clusterName: string }) {
  const { data } = useQuery(eksClusterQueryOptions(clusterName))

  return <StateBadge state={data?.cluster?.status} />
}

function ClusterVersion({ clusterName }: { clusterName: string }) {
  const { data } = useQuery(eksClusterQueryOptions(clusterName))

  return data?.cluster?.version ?? "—"
}

// DataTable rows are objects, so each name gets a wrapper.
interface ClusterRow {
  name: string
}

const COLUMNS: DataColumn<ClusterRow>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: ({ name }) => name,
    cell: ({ name }) => (
      <Link
        className="block max-w-72 min-w-0 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/eks/list-clusters/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: name,
        })}
        to="."
      >
        {name}
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    cell: ({ name }) => <ClusterStatus clusterName={name} />,
  },
  {
    id: "version",
    header: "Kubernetes version",
    hideBelow: "sm",
    cell: ({ name }) => <ClusterVersion clusterName={name} />,
  },
]

interface ClustersListPageProps {
  clusters: readonly string[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function ClustersListPage({
  clusters,
  search,
  onSearchChange,
}: ClustersListPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })
  const rows = useMemo(() => clusters.map((name) => ({ name })), [clusters])

  const open = clusters.find(
    (name) => search.selected !== "" && name === search.selected,
  )

  let panel: ReactNode = null

  if (open) {
    panel = (
      <ClusterDetails
        clusterName={open}
        key={open}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
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
          <Link className={buttonVariants()} to="/eks/create-cluster">
            <Plus data-icon="inline-start" />
            Create cluster
          </Link>
        }
        subtitle="Managed Kubernetes control planes and their node groups."
        title="Clusters"
      />
      <DataTable
        activeId={open}
        caption="EKS clusters"
        columns={COLUMNS}
        empty="No EKS clusters found."
        getRowId={({ name }) => name}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={rows}
        sort={sort}
      />
    </ResourceView>
  )
}
