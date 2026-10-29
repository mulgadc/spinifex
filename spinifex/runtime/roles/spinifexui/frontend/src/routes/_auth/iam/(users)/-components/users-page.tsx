import type { User } from "@aws-sdk/client-iam"
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

import { UserDetails } from "./user-details"

const COLUMNS: DataColumn<User>[] = [
  {
    id: "name",
    header: "User name",
    sortValue: (user) => user.UserName,
    cell: (user) => (
      <Link
        className="block max-w-72 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/iam/list-users/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: user.UserName ?? "",
        })}
        to="."
      >
        {user.UserName}
      </Link>
    ),
  },
  {
    id: "id",
    header: "User ID",
    sortValue: (user) => user.UserId,
    hideBelow: "md",
    cell: (user) => <span className="font-mono text-xs">{user.UserId}</span>,
  },
  {
    id: "path",
    header: "Path",
    sortValue: (user) => user.Path,
    hideBelow: "lg",
    cell: (user) => <span className="font-mono text-xs">{user.Path}</span>,
  },
  {
    id: "created",
    header: "Created",
    sortValue: (user) => user.CreateDate?.getTime(),
    hideBelow: "lg",
    cell: (user) => formatDateTime(user.CreateDate),
  },
]

interface UsersPageProps {
  users: readonly User[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function UsersPage({ users, search, onSearchChange }: UsersPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = users.find(
    (user) => search.selected !== "" && user.UserName === search.selected,
  )

  return (
    <ResourceView
      panel={
        open && (
          <UserDetails
            key={open.UserName}
            onClose={() => {
              onSearchChange(CLOSED_PANEL)
            }}
            onTabChange={(tab) => {
              onSearchChange({ tab })
            }}
            tab={search.tab}
            user={open}
          />
        )
      }
    >
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/iam/create-user">
            <Plus data-icon="inline-start" />
            Create user
          </Link>
        }
        subtitle="People and workloads that sign requests with their own credentials."
        title="Users"
      />
      <DataTable
        activeId={open?.UserName}
        caption="Users"
        columns={COLUMNS}
        empty="No IAM users found."
        getRowId={(user) => user.UserName ?? ""}
        onRowClick={(userName) => {
          onSearchChange({ ...CLOSED_PANEL, selected: userName })
        }}
        onSortChange={setSort}
        rows={users}
        sort={sort}
      />
    </ResourceView>
  )
}
