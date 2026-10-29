import type { DBInstance } from "@aws-sdk/client-rds"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
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
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { useDeleteDBInstance } from "@/mutations/rds"

import { DBInstanceDetails } from "./db-instance-details"
import { defaultFinalSnapshotIdentifier } from "./delete-db-instance-dialog"

function engineLabel(instance: DBInstance): string {
  return [instance.Engine, instance.EngineVersion].filter(Boolean).join(" ")
}

const COLUMNS: DataColumn<DBInstance>[] = [
  {
    id: "identifier",
    header: "Identifier",
    sortValue: (instance) => instance.DBInstanceIdentifier,
    cell: (instance) => (
      <Link
        className="block max-w-72 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/rds/describe-db-instances/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: instance.DBInstanceIdentifier ?? "",
        })}
        to="."
      >
        {instance.DBInstanceIdentifier}
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    sortValue: (instance) => instance.DBInstanceStatus,
    cell: (instance) => <StateBadge state={instance.DBInstanceStatus} />,
  },
  {
    id: "engine",
    header: "Engine",
    sortValue: engineLabel,
    hideBelow: "sm",
    cell: (instance) => engineLabel(instance) || "—",
  },
  {
    id: "class",
    header: "Class",
    sortValue: (instance) => instance.DBInstanceClass,
    hideBelow: "md",
    cell: (instance) => instance.DBInstanceClass ?? "—",
  },
  {
    id: "endpoint",
    header: "Endpoint",
    sortValue: (instance) => instance.Endpoint?.Address,
    hideBelow: "lg",
    cell: (instance) => (
      <span className="font-mono text-xs">
        {instance.Endpoint?.Address ?? "—"}
      </span>
    ),
  },
  {
    id: "storage",
    header: "Storage",
    sortValue: (instance) => instance.AllocatedStorage,
    hideBelow: "xl",
    align: "end",
    cell: (instance) =>
      instance.AllocatedStorage ? `${instance.AllocatedStorage} GiB` : "—",
  },
]

interface DBInstancesListPageProps {
  instances: readonly DBInstance[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function DBInstancesListPage({
  instances,
  search,
  onSearchChange,
}: DBInstancesListPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = instances.find(
    (instance) =>
      search.selected !== "" &&
      instance.DBInstanceIdentifier === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const selection = useRowSelection(
    instances.map((instance) => instance.DBInstanceIdentifier ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteInstance = useDeleteDBInstance()

  // The single delete's default: a final snapshot under its suggested name.
  // Skipping it means typing the instance's name, so that stays per instance.
  async function deleteWithFinalSnapshot(id: string) {
    return await deleteInstance.mutateAsync({
      dbInstanceIdentifier: id,
      skipFinalSnapshot: false,
      finalSnapshotIdentifier: defaultFinalSnapshotIdentifier(id),
    })
  }

  let panel: ReactNode = null

  if (open?.DBInstanceIdentifier) {
    panel = (
      <DBInstanceDetails
        instance={open}
        key={open.DBInstanceIdentifier}
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
          <Link className={buttonVariants()} to="/rds/create-db-instance">
            <Plus data-icon="inline-start" />
            Create database
          </Link>
        }
        subtitle="Managed PostgreSQL and MariaDB instances."
        title="Databases"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteWithFinalSnapshot}
            description="This tears them down, each after taking a final snapshot named after it, which keeps its data until that snapshot is deleted. To delete without a final snapshot, delete an instance on its own."
            ids={selection.ids}
            noun={["DB instance", "DB instances"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.DBInstanceIdentifier}
        caption="Databases"
        columns={COLUMNS}
        empty="No DB instances found."
        getRowId={(instance) => instance.DBInstanceIdentifier ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={instances}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
