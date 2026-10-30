import type { HostedZone } from "@aws-sdk/client-route-53"
import { useQuery, useQueryClient } from "@tanstack/react-query"
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
import { ActivationBadge } from "@/components/route53/activation-badge"
import { SelectionBar } from "@/components/selection-bar"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { bareZoneId } from "@/lib/route53-activation"
import { useDeleteHostedZone } from "@/mutations/route53"
import { route53ActivationQueryOptions } from "@/queries/route53"

import { CreateHostedZonePage } from "./create-hosted-zone-page"
import { deleteHostedZone } from "./delete-hosted-zone"
import { HostedZoneDetails } from "./hosted-zone-details"

function ZoneActivation({ zone }: { zone: HostedZone }) {
  const { data, isPending } = useQuery(
    route53ActivationQueryOptions(bareZoneId(zone.Id ?? "")),
  )

  return (
    <ActivationBadge
      fallback={isPending ? "Loading…" : "Unknown"}
      state={data?.state}
    />
  )
}

const COLUMNS: DataColumn<HostedZone>[] = [
  {
    id: "name",
    header: "Domain name",
    sortValue: (zone) => zone.Name,
    cell: (zone) => (
      <Link
        className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/route53/list-hosted-zones/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: bareZoneId(zone.Id ?? ""),
        })}
        to="."
      >
        <span className="block truncate font-medium text-primary group-hover:underline">
          {zone.Name}
        </span>
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    cell: (zone) => <ZoneActivation zone={zone} />,
  },
  {
    id: "records",
    header: "Records",
    sortValue: (zone) => zone.ResourceRecordSetCount,
    hideBelow: "sm",
    align: "end",
    cell: (zone) => (
      <span className="tabular-nums">{zone.ResourceRecordSetCount ?? "—"}</span>
    ),
  },
  {
    id: "comment",
    header: "Comment",
    sortValue: (zone) => zone.Config?.Comment,
    hideBelow: "lg",
    cell: (zone) => (
      <span className="text-muted-foreground">{zone.Config?.Comment}</span>
    ),
  },
  {
    id: "id",
    header: "Hosted zone ID",
    sortValue: (zone) => bareZoneId(zone.Id ?? ""),
    hideBelow: "md",
    cell: (zone) => (
      <span className="font-mono text-xs">{bareZoneId(zone.Id ?? "")}</span>
    ),
  },
]

interface ListHostedZonesPageProps {
  zones: readonly HostedZone[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function ListHostedZonesPage({
  zones,
  search,
  onSearchChange,
}: ListHostedZonesPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = zones.find(
    (zone) =>
      search.selected !== "" && bareZoneId(zone.Id ?? "") === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const selection = useRowSelection(
    zones.map((zone) => bareZoneId(zone.Id ?? "")),
    open !== undefined,
    closePanel,
  )

  const queryClient = useQueryClient()
  const remove = useDeleteHostedZone()

  const names = new Map(
    zones.map((zone) => [bareZoneId(zone.Id ?? ""), zone.Name]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel onClose={closePanel} title="Add domain">
        <CreateHostedZonePage
          onClose={(zoneId) => {
            onSearchChange({ ...CLOSED_PANEL, selected: zoneId ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open) {
    panel = (
      <HostedZoneDetails
        key={open.Id}
        onClose={closePanel}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        tab={search.tab}
        zone={open}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/route53/list-hosted-zones/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Add domain
          </Link>
        }
        subtitle="Domains whose DNS this cluster serves"
        title="Hosted zones"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            confirmPhrase="delete"
            deleteOne={async (id) =>
              await deleteHostedZone(
                queryClient,
                id,
                names.get(id) ?? id,
                remove.mutateAsync,
              )
            }
            description="Each zone's record sets besides NS and SOA are deleted first. This cannot be undone."
            ids={selection.ids}
            label={(id) => `${names.get(id) ?? id} (${id})`}
            noun={["hosted zone", "hosted zones"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open ? bareZoneId(open.Id ?? "") : undefined}
        caption="Hosted zones"
        columns={COLUMNS}
        empty={
          <div className="space-y-2">
            <p>No hosted zones yet.</p>
            <p className="text-sm">
              Add a domain to serve its DNS from this cluster. You will get the
              name servers to set at your registrar.
            </p>
          </div>
        }
        getRowId={(zone) => bareZoneId(zone.Id ?? "")}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={zones}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
