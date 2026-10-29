import type { Role } from "@aws-sdk/client-iam"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { useState } from "react"

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
import { formatDateTime } from "@/lib/utils"
import { useDeleteRole } from "@/mutations/iam"

import { RoleDetails } from "./role-details"

const COLUMNS: DataColumn<Role>[] = [
  {
    id: "name",
    header: "Role name",
    sortValue: (role) => role.RoleName,
    cell: (role) => (
      <Link
        className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/iam/list-roles/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: role.RoleName ?? "",
        })}
        to="."
      >
        <span className="block truncate font-medium text-primary group-hover:underline">
          {role.RoleName}
        </span>
      </Link>
    ),
  },
  {
    id: "description",
    header: "Description",
    sortValue: (role) => role.Description,
    hideBelow: "md",
    cell: (role) => (
      <span className="block max-w-80 truncate text-muted-foreground">
        {role.Description}
      </span>
    ),
  },
  {
    id: "created",
    header: "Created",
    sortValue: (role) => role.CreateDate?.getTime(),
    hideBelow: "lg",
    cell: (role) => formatDateTime(role.CreateDate),
  },
]

interface RolesPageProps {
  roles: readonly Role[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function RolesPage({ roles, search, onSearchChange }: RolesPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = roles.find(
    (role) => search.selected !== "" && role.RoleName === search.selected,
  )

  const selection = useRowSelection(
    roles.map((role) => role.RoleName ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const remove = useDeleteRole()

  const resourceIds = new Map(roles.map((role) => [role.RoleName, role.RoleId]))

  return (
    <ResourceView
      panel={
        open && (
          <RoleDetails
            key={open.RoleName}
            onClose={() => {
              onSearchChange(CLOSED_PANEL)
            }}
            onTabChange={(tab) => {
              onSearchChange({ tab })
            }}
            role={open}
            tab={search.tab}
          />
        )
      }
    >
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/iam/create-role">
            <Plus data-icon="inline-start" />
            Create role
          </Link>
        }
        subtitle="Identities that services and workloads assume for temporary credentials."
        title="Roles"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={remove.mutateAsync}
            description="Detach their policies and remove them from their instance profiles first, or the delete is refused. This cannot be undone."
            ids={selection.ids}
            label={(id) => `${id} (${resourceIds.get(id) ?? ""})`}
            noun={["role", "roles"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.RoleName}
        caption="Roles"
        columns={COLUMNS}
        empty="No IAM roles found."
        getRowId={(role) => role.RoleName ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={roles}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
