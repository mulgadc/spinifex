import type { DBSubnetGroup } from "@aws-sdk/client-rds"
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
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { useDeleteDBSubnetGroup } from "@/mutations/rds"

import { CreateDBSubnetGroupPage } from "./create-db-subnet-group-page"
import { DBSubnetGroupDetails } from "./db-subnet-group-details"

const COLUMNS: DataColumn<DBSubnetGroup>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (group) => group.DBSubnetGroupName,
    cell: (group) => (
      <Link
        className="block max-w-72 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/rds/describe-db-subnet-groups/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: group.DBSubnetGroupName ?? "",
        })}
        to="."
      >
        {group.DBSubnetGroupName}
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    sortValue: (group) => group.SubnetGroupStatus,
    cell: (group) => <StateBadge state={group.SubnetGroupStatus} />,
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (group) => group.VpcId,
    hideBelow: "sm",
    cell: (group) => (
      <span className="font-mono text-xs">{group.VpcId ?? "—"}</span>
    ),
  },
  {
    id: "subnets",
    header: "Subnets",
    sortValue: (group) => group.Subnets?.length ?? 0,
    hideBelow: "md",
    cell: (group) => (
      <span className="tabular-nums">{group.Subnets?.length ?? 0}</span>
    ),
  },
  {
    id: "description",
    header: "Description",
    sortValue: (group) => group.DBSubnetGroupDescription,
    hideBelow: "lg",
    cell: (group) => (
      <span className="text-muted-foreground">
        {group.DBSubnetGroupDescription}
      </span>
    ),
  },
]

interface DBSubnetGroupsListPageProps {
  groups: readonly DBSubnetGroup[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function DBSubnetGroupsListPage({
  groups,
  search,
  onSearchChange,
}: DBSubnetGroupsListPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = groups.find(
    (group) =>
      search.selected !== "" && group.DBSubnetGroupName === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const selection = useRowSelection(
    groups.map((group) => group.DBSubnetGroupName ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteGroup = useDeleteDBSubnetGroup()

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel onClose={closePanel} title="Create DB subnet group">
        <CreateDBSubnetGroupPage
          onClose={(id) => {
            onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.DBSubnetGroupName) {
    panel = (
      <DBSubnetGroupDetails
        group={open}
        key={open.DBSubnetGroupName}
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
            from="/rds/describe-db-subnet-groups/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create Subnet Group
          </Link>
        }
        title="DB Subnet Groups"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteGroup.mutateAsync}
            description="This deletes the DB subnet groups. Each is refused while any DB instance still references it, including one that is only deleting."
            ids={selection.ids}
            noun={["DB subnet group", "DB subnet groups"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.DBSubnetGroupName}
        caption="DB subnet groups"
        columns={COLUMNS}
        empty="No DB subnet groups found. Without one, a database is placed in the account's default VPC subnet."
        getRowId={(group) => group.DBSubnetGroupName ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={groups}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
