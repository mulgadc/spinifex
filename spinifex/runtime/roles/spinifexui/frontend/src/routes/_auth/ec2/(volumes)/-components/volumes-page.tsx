import type { Volume } from "@aws-sdk/client-ec2"
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
import { formatDateTime, getNameTag } from "@/lib/utils"
import { useDeleteVolume } from "@/mutations/ec2"

import { VolumeDetails } from "./volume-details"

const COLUMNS: DataColumn<Volume>[] = [
  {
    id: "volume",
    header: "Volume",
    sortValue: (volume) => getNameTag(volume.Tags) ?? volume.VolumeId,
    cell: (volume) => {
      const name = getNameTag(volume.Tags)
      const id = volume.VolumeId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-volumes/"
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
    sortValue: (volume) => volume.State,
    cell: (volume) => <StateBadge state={volume.State} />,
  },
  {
    id: "size",
    header: "Size",
    sortValue: (volume) => volume.Size,
    align: "end",
    cell: (volume) => <span className="tabular-nums">{volume.Size} GiB</span>,
  },
  {
    id: "type",
    header: "Type",
    sortValue: (volume) => volume.VolumeType,
    hideBelow: "md",
    cell: (volume) => volume.VolumeType,
  },
  {
    id: "instance",
    header: "Attached to",
    sortValue: (volume) => volume.Attachments?.[0]?.InstanceId,
    hideBelow: "sm",
    cell: (volume) => {
      const instanceId = volume.Attachments?.[0]?.InstanceId

      return instanceId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: instanceId }}
          to="/ec2/describe-instances/$id"
        >
          {instanceId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      )
    },
  },
  {
    id: "created",
    header: "Created",
    sortValue: (volume) => volume.CreateTime?.getTime(),
    hideBelow: "xl",
    cell: (volume) => (
      <span className="tabular-nums">{formatDateTime(volume.CreateTime)}</span>
    ),
  },
]

interface VolumesPageProps {
  volumes: readonly Volume[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function VolumesPage({
  volumes,
  search,
  onSearchChange,
}: VolumesPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = volumes.find(
    (volume) => search.selected !== "" && volume.VolumeId === search.selected,
  )

  const selection = useRowSelection(
    volumes.map((volume) => volume.VolumeId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteVolume = useDeleteVolume()

  const names = new Map(
    volumes.map((volume) => [volume.VolumeId, getNameTag(volume.Tags)]),
  )

  let panel: ReactNode = null

  if (open?.VolumeId) {
    panel = (
      <VolumeDetails
        key={open.VolumeId}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        tab={search.tab}
        volume={open}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/ec2/create-volume">
            <Plus data-icon="inline-start" />
            Create volume
          </Link>
        }
        subtitle="Block storage for instances, attached one at a time."
        title="Volumes"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteVolume.mutateAsync}
            description="Their data is lost unless they have a snapshot. This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["volume", "volumes"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.VolumeId}
        caption="Volumes"
        columns={COLUMNS}
        empty="No volumes yet."
        getRowId={(volume) => volume.VolumeId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={volumes}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
