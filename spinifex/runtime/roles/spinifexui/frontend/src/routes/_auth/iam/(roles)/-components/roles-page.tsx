import type { Role } from "@aws-sdk/client-iam"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"

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
      <DataTable
        activeId={open?.RoleName}
        caption="Roles"
        columns={COLUMNS}
        empty="No IAM roles found."
        getRowId={(role) => role.RoleName ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
        }}
        onSortChange={setSort}
        rows={roles}
        sort={sort}
      />
    </ResourceView>
  )
}
