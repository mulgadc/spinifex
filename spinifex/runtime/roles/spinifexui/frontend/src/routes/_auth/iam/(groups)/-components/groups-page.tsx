import type { Group } from "@aws-sdk/client-iam"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { useState } from "react"

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
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteGroup } from "@/mutations/iam"

import { iamDeleteWarning } from "../../-components/delete-prerequisites"
import { CreateGroupPage } from "./create-group-page"
import { GroupDetails } from "./group-details"

const COLUMNS: DataColumn<Group>[] = [
  {
    id: "name",
    header: "Group name",
    sortValue: (group) => group.GroupName,
    cell: (group) => (
      <Link
        className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/iam/list-groups/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: group.GroupName ?? "",
        })}
        to="."
      >
        <span className="block truncate font-medium text-primary group-hover:underline">
          {group.GroupName}
        </span>
      </Link>
    ),
  },
  {
    id: "id",
    header: "Group ID",
    sortValue: (group) => group.GroupId,
    hideBelow: "md",
    cell: (group) => <span className="font-mono text-xs">{group.GroupId}</span>,
  },
  {
    id: "created",
    header: "Created",
    sortValue: (group) => group.CreateDate?.getTime(),
    hideBelow: "lg",
    cell: (group) => formatDateTime(group.CreateDate),
  },
]

interface GroupsPageProps {
  groups: readonly Group[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function GroupsPage({
  groups,
  search,
  onSearchChange,
}: GroupsPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = groups.find(
    (group) => search.selected !== "" && group.GroupName === search.selected,
  )

  const selection = useRowSelection(
    groups.map((group) => group.GroupName ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const remove = useDeleteGroup()

  const resourceIds = new Map(
    groups.map((group) => [group.GroupName, group.GroupId]),
  )

  return (
    <ResourceView
      panel={
        search.create ? (
          <CreatePanel
            onClose={() => {
              onSearchChange(CLOSED_PANEL)
            }}
            title="Create group"
          >
            <CreateGroupPage
              onClose={(id) => {
                onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
              }}
            />
          </CreatePanel>
        ) : (
          open && (
            <GroupDetails
              key={open.GroupName}
              onClose={() => {
                onSearchChange(CLOSED_PANEL)
              }}
              onTabChange={(tab) => {
                onSearchChange({ tab })
              }}
              group={open}
              tab={search.tab}
            />
          )
        )
      }
    >
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/iam/list-groups/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create group
          </Link>
        }
        subtitle="Collections of users that share the policies attached to them."
        title="Groups"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={remove.mutateAsync}
            description={iamDeleteWarning("group", selection.ids.length)}
            ids={selection.ids}
            label={(id) => `${id} (${resourceIds.get(id) ?? ""})`}
            noun={["group", "groups"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.GroupName}
        caption="Groups"
        columns={COLUMNS}
        empty="No IAM groups found."
        getRowId={(group) => group.GroupName ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
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
