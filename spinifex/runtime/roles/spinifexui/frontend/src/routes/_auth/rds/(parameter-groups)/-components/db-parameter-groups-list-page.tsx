import type { DBParameterGroup } from "@aws-sdk/client-rds"
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
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { useDeleteDBParameterGroup } from "@/mutations/rds"
import { isDefaultParameterGroupName } from "@/types/rds"

import { DBParameterGroupDetails } from "./db-parameter-group-details"

function groupType(group: DBParameterGroup): string {
  return isDefaultParameterGroupName(group.DBParameterGroupName ?? "")
    ? "Default"
    : "Customer"
}

const COLUMNS: DataColumn<DBParameterGroup>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (group) => group.DBParameterGroupName,
    cell: (group) => (
      <Link
        className="block max-w-72 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/rds/describe-db-parameter-groups/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: group.DBParameterGroupName ?? "",
        })}
        to="."
      >
        {group.DBParameterGroupName}
      </Link>
    ),
  },
  {
    id: "family",
    header: "Family",
    sortValue: (group) => group.DBParameterGroupFamily,
    cell: (group) => (
      <span className="font-mono text-xs">{group.DBParameterGroupFamily}</span>
    ),
  },
  {
    id: "type",
    header: "Type",
    sortValue: groupType,
    hideBelow: "sm",
    cell: (group) => (
      <span className="text-muted-foreground">{groupType(group)}</span>
    ),
  },
  {
    id: "description",
    header: "Description",
    sortValue: (group) => group.Description,
    hideBelow: "lg",
    cell: (group) => (
      <span className="text-muted-foreground">{group.Description}</span>
    ),
  },
]

interface DBParameterGroupsListPageProps {
  groups: readonly DBParameterGroup[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function DBParameterGroupsListPage({
  groups,
  search,
  onSearchChange,
}: DBParameterGroupsListPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = groups.find(
    (group) =>
      search.selected !== "" && group.DBParameterGroupName === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const selection = useRowSelection(
    groups.map((group) => group.DBParameterGroupName ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteGroup = useDeleteDBParameterGroup()

  let panel: ReactNode = null

  if (open?.DBParameterGroupName) {
    panel = (
      <DBParameterGroupDetails
        group={open}
        key={open.DBParameterGroupName}
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
            to="/rds/create-db-parameter-group"
          >
            <Plus data-icon="inline-start" />
            Create Parameter Group
          </Link>
        }
        title="DB Parameter Groups"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteGroup.mutateAsync}
            description="This deletes the DB parameter groups and every value stored on them. Each is refused while any DB instance references it, including one whose pending changes name it."
            ids={selection.ids}
            noun={["DB parameter group", "DB parameter groups"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.DBParameterGroupName}
        caption="DB parameter groups"
        columns={COLUMNS}
        empty="No DB parameter groups found."
        getRowId={(group) => group.DBParameterGroupName ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={groups}
        selected={selection.selected}
        sort={sort}
      />
      <p className="mt-4 text-xs text-muted-foreground">
        A default group is owned by the service. It is readable and attachable
        but cannot be modified, deleted or tagged — create your own group to
        change a value.
      </p>
    </ResourceView>
  )
}
