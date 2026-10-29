import type { Snapshot } from "@aws-sdk/client-ec2"
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
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime, getNameTag } from "@/lib/utils"

import { SnapshotDetails } from "./snapshot-details"

const COLUMNS: DataColumn<Snapshot>[] = [
  {
    id: "snapshot",
    header: "Snapshot",
    sortValue: (snapshot) => getNameTag(snapshot.Tags) ?? snapshot.SnapshotId,
    cell: (snapshot) => {
      const name = getNameTag(snapshot.Tags)
      const id = snapshot.SnapshotId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-snapshots/"
          search={(previous) => ({
            ...previous,
            ...CLOSED_PANEL,
            selected: id,
          })}
          to="."
        >
          <span className="block truncate font-medium text-primary group-hover:underline">
            {name ?? id}
          </span>
          {name && (
            <span className="block truncate font-mono text-xs text-muted-foreground">
              {id}
            </span>
          )}
        </Link>
      )
    },
  },
  {
    id: "state",
    header: "State",
    sortValue: (snapshot) => snapshot.State,
    cell: (snapshot) => <StateBadge state={snapshot.State} />,
  },
  {
    id: "size",
    header: "Size",
    sortValue: (snapshot) => snapshot.VolumeSize,
    align: "end",
    cell: (snapshot) => (
      <span className="tabular-nums">{snapshot.VolumeSize} GiB</span>
    ),
  },
  {
    id: "volume",
    header: "Volume",
    sortValue: (snapshot) => snapshot.VolumeId,
    hideBelow: "sm",
    cell: (snapshot) =>
      snapshot.VolumeId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: snapshot.VolumeId }}
          to="/ec2/describe-volumes/$id"
        >
          {snapshot.VolumeId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "description",
    header: "Description",
    sortValue: (snapshot) => snapshot.Description,
    hideBelow: "lg",
    cell: (snapshot) => (
      <span className="block max-w-80 truncate" title={snapshot.Description}>
        {snapshot.Description}
      </span>
    ),
  },
  {
    id: "started",
    header: "Started",
    sortValue: (snapshot) => snapshot.StartTime?.getTime(),
    hideBelow: "xl",
    cell: (snapshot) => (
      <span className="tabular-nums">{formatDateTime(snapshot.StartTime)}</span>
    ),
  },
]

interface SnapshotsPageProps {
  snapshots: readonly Snapshot[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function SnapshotsPage({
  snapshots,
  search,
  onSearchChange,
}: SnapshotsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = snapshots.find(
    (snapshot) =>
      search.selected !== "" && snapshot.SnapshotId === search.selected,
  )

  let panel: ReactNode = null

  if (open?.SnapshotId) {
    panel = (
      <SnapshotDetails
        key={open.SnapshotId}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
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
          <Link className={buttonVariants()} to="/ec2/create-snapshot">
            <Plus data-icon="inline-start" />
            Create snapshot
          </Link>
        }
        subtitle="Point-in-time copies of volumes, to restore or register as AMIs."
        title="Snapshots"
      />
      <DataTable
        activeId={open?.SnapshotId}
        caption="Snapshots"
        columns={COLUMNS}
        empty="No snapshots yet."
        getRowId={(snapshot) => snapshot.SnapshotId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={snapshots}
        sort={sort}
      />
    </ResourceView>
  )
}
