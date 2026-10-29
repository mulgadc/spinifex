import type { PlacementGroup } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, Suspense, useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { DetailPanel, PanelSpinner } from "@/components/detail-panel"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { SelectionBar } from "@/components/selection-bar"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { useDeletePlacementGroup } from "@/mutations/ec2"

import { CreatePlacementGroupPage } from "./create-placement-group-page"
import { PlacementGroupDetails } from "./placement-group-details"

const COLUMNS: DataColumn<PlacementGroup>[] = [
  {
    id: "group",
    header: "Placement group",
    sortValue: (pg) => pg.GroupName ?? pg.GroupId,
    cell: (pg) => {
      const id = pg.GroupId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-placement-groups/"
          search={(previous) => ({
            ...previous,
            ...CLOSED_PANEL,
            selected: id,
          })}
          to="."
        >
          <span className="block truncate font-medium text-primary group-hover:underline">
            {pg.GroupName ?? id}
          </span>
          {pg.GroupName && (
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
    sortValue: (pg) => pg.State,
    cell: (pg) => <StateBadge state={pg.State} />,
  },
  {
    id: "strategy",
    header: "Strategy",
    sortValue: (pg) => pg.Strategy,
    hideBelow: "sm",
    cell: (pg) => pg.Strategy,
  },
  {
    id: "spread",
    header: "Spread level",
    sortValue: (pg) => pg.SpreadLevel,
    hideBelow: "lg",
    cell: (pg) => pg.SpreadLevel ?? "—",
  },
]

interface PlacementGroupsPageProps {
  placementGroups: readonly PlacementGroup[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function PlacementGroupsPage({
  placementGroups,
  search,
  onSearchChange,
}: PlacementGroupsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = placementGroups.find(
    (pg) => search.selected !== "" && pg.GroupId === search.selected,
  )

  const selection = useRowSelection(
    placementGroups.map((pg) => pg.GroupId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deletePlacementGroup = useDeletePlacementGroup()

  const names = new Map(placementGroups.map((pg) => [pg.GroupId, pg.GroupName]))

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <DetailPanel
        closeLabel="Close create form"
        label="Create placement group"
        onClose={closePanel}
        title="Create placement group"
      >
        <Suspense fallback={<PanelSpinner />}>
          <CreatePlacementGroupPage
            onClose={(groupId) => {
              onSearchChange({ ...CLOSED_PANEL, selected: groupId ?? "" })
            }}
          />
        </Suspense>
      </DetailPanel>
    )
  } else if (open?.GroupId) {
    panel = (
      <PlacementGroupDetails
        key={open.GroupId}
        onClose={closePanel}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        pg={open}
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
            from="/ec2/describe-placement-groups/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create placement group
          </Link>
        }
        subtitle="Control how instances are spread across hosts."
        title="Placement groups"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            // DeletePlacementGroup takes the group's name, not its ID.
            deleteOne={async (id) =>
              await deletePlacementGroup.mutateAsync(names.get(id) ?? "")
            }
            description="They must have no instances in them. This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["placement group", "placement groups"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.GroupId}
        caption="Placement groups"
        columns={COLUMNS}
        empty="No placement groups yet."
        getRowId={(pg) => pg.GroupId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={placementGroups}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
