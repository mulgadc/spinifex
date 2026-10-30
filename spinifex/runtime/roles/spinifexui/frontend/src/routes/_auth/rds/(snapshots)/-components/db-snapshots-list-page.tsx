import type { DBSnapshot } from "@aws-sdk/client-rds"
import { Link } from "@tanstack/react-router"
import { Camera } from "lucide-react"
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
import { Button } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteDBSnapshot } from "@/mutations/rds"
import { SNAPSHOT_TYPE_AUTOMATED, SNAPSHOT_TYPE_MANUAL } from "@/types/rds"

import { CreateDBSnapshotDialog } from "./create-db-snapshot-dialog"
import { DBSnapshotDetails } from "./db-snapshot-details"

const TYPE_FILTERS = [
  { value: "", label: "All types" },
  { value: SNAPSHOT_TYPE_MANUAL, label: "Manual" },
  { value: SNAPSHOT_TYPE_AUTOMATED, label: "Automated" },
] as const

function engineLabel(snapshot: DBSnapshot): string {
  return [snapshot.Engine, snapshot.EngineVersion].filter(Boolean).join(" ")
}

const COLUMNS: DataColumn<DBSnapshot>[] = [
  {
    id: "identifier",
    header: "Identifier",
    sortValue: (snapshot) => snapshot.DBSnapshotIdentifier,
    cell: (snapshot) => (
      <Link
        className="block max-w-72 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/rds/describe-db-snapshots/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: snapshot.DBSnapshotIdentifier ?? "",
        })}
        to="."
      >
        {snapshot.DBSnapshotIdentifier}
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    sortValue: (snapshot) => snapshot.Status,
    cell: (snapshot) => <StateBadge state={snapshot.Status} />,
  },
  {
    id: "instance",
    header: "DB instance",
    sortValue: (snapshot) => snapshot.DBInstanceIdentifier,
    hideBelow: "sm",
    cell: (snapshot) => {
      const sourceId = snapshot.DBInstanceIdentifier ?? ""

      return sourceId === "" ? (
        "—"
      ) : (
        <Link
          className="text-primary hover:underline"
          params={{ id: sourceId }}
          to="/rds/describe-db-instances/$id"
        >
          {sourceId}
        </Link>
      )
    },
  },
  {
    id: "type",
    header: "Type",
    sortValue: (snapshot) => snapshot.SnapshotType,
    hideBelow: "md",
    cell: (snapshot) => snapshot.SnapshotType ?? "—",
  },
  {
    id: "created",
    header: "Created",
    sortValue: (snapshot) => snapshot.SnapshotCreateTime?.getTime(),
    hideBelow: "md",
    cell: (snapshot) => (
      <span className="font-mono text-xs">
        {formatDateTime(snapshot.SnapshotCreateTime)}
      </span>
    ),
  },
  {
    id: "engine",
    header: "Engine",
    sortValue: engineLabel,
    hideBelow: "lg",
    cell: (snapshot) => engineLabel(snapshot) || "—",
  },
  {
    id: "storage",
    header: "Storage",
    sortValue: (snapshot) => snapshot.AllocatedStorage,
    hideBelow: "xl",
    align: "end",
    cell: (snapshot) =>
      snapshot.AllocatedStorage ? `${snapshot.AllocatedStorage} GiB` : "—",
  },
]

interface DBSnapshotsListPageProps {
  snapshots: readonly DBSnapshot[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function DBSnapshotsListPage({
  snapshots,
  search,
  onSearchChange,
}: DBSnapshotsListPageProps) {
  const [sort, setSort] = useState<SortState>()
  const [typeFilter, setTypeFilter] = useState<string>("")
  const [showCreate, setShowCreate] = useState(false)

  const visible = snapshots.filter(
    (snapshot) => typeFilter === "" || snapshot.SnapshotType === typeFilter,
  )

  // Looked up in every snapshot, so a filter never closes an open panel.
  const open = snapshots.find(
    (snapshot) =>
      search.selected !== "" &&
      snapshot.DBSnapshotIdentifier === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const selection = useRowSelection(
    visible.map((snapshot) => snapshot.DBSnapshotIdentifier ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteSnapshot = useDeleteDBSnapshot()

  let panel: ReactNode = null

  if (open?.DBSnapshotIdentifier) {
    panel = (
      <DBSnapshotDetails
        key={open.DBSnapshotIdentifier}
        onClose={closePanel}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        snapshot={open}
        tab={search.tab}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Button
            onClick={() => {
              setShowCreate(true)
            }}
          >
            <Camera data-icon="inline-start" />
            Take snapshot
          </Button>
        }
        subtitle="Manual snapshots and automated backups of your databases."
        title="Snapshots"
      />

      <div className="mb-4 flex items-center gap-2">
        {TYPE_FILTERS.map((filter) => (
          <Button
            key={filter.value}
            onClick={() => {
              setTypeFilter(filter.value)
            }}
            size="sm"
            variant={typeFilter === filter.value ? "default" : "outline"}
          >
            {filter.label}
          </Button>
        ))}
      </div>

      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            confirmPhrase="delete"
            deleteOne={deleteSnapshot.mutateAsync}
            description="This deletes the DB snapshots and the data behind them. Each is refused while an instance restored from it still exists. Deleting a final snapshot also releases the data volume its instance left behind."
            ids={selection.ids}
            noun={["DB snapshot", "DB snapshots"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}

      <DataTable
        activeId={open?.DBSnapshotIdentifier}
        caption="DB snapshots"
        columns={COLUMNS}
        empty="No DB snapshots found."
        getRowId={(snapshot) => snapshot.DBSnapshotIdentifier ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={visible}
        selected={selection.selected}
        sort={sort}
      />

      <p className="mt-4 text-xs text-muted-foreground">
        Automated backups live in the reserved rds: namespace and cannot be
        deleted by hand — the instance&apos;s backup retention is what removes
        them.
      </p>

      {showCreate && (
        <CreateDBSnapshotDialog onOpenChange={setShowCreate} open={true} />
      )}
    </ResourceView>
  )
}
