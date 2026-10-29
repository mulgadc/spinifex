import type { InstanceProfile } from "@aws-sdk/client-iam"
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

import { InstanceProfileDetails } from "./instance-profile-details"

const COLUMNS: DataColumn<InstanceProfile>[] = [
  {
    id: "name",
    header: "Instance profile",
    sortValue: (profile) => profile.InstanceProfileName,
    cell: (profile) => (
      <Link
        className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/iam/list-instance-profiles/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: profile.InstanceProfileName ?? "",
        })}
        to="."
      >
        <span className="block truncate font-medium text-primary group-hover:underline">
          {profile.InstanceProfileName}
        </span>
      </Link>
    ),
  },
  {
    id: "role",
    header: "Role",
    sortValue: (profile) => profile.Roles?.[0]?.RoleName,
    hideBelow: "md",
    cell: (profile) => {
      const roleName = profile.Roles?.[0]?.RoleName

      return roleName ? (
        <Link
          className="text-primary hover:underline"
          params={{ roleName }}
          to="/iam/list-roles/$roleName"
        >
          {roleName}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      )
    },
  },
  {
    id: "created",
    header: "Created",
    sortValue: (profile) => profile.CreateDate?.getTime(),
    hideBelow: "lg",
    cell: (profile) => formatDateTime(profile.CreateDate),
  },
]

interface InstanceProfilesPageProps {
  instanceProfiles: readonly InstanceProfile[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function InstanceProfilesPage({
  instanceProfiles,
  search,
  onSearchChange,
}: InstanceProfilesPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = instanceProfiles.find(
    (profile) =>
      search.selected !== "" && profile.InstanceProfileName === search.selected,
  )

  return (
    <ResourceView
      panel={
        open && (
          <InstanceProfileDetails
            key={open.InstanceProfileName}
            onClose={() => {
              onSearchChange(CLOSED_PANEL)
            }}
            onTabChange={(tab) => {
              onSearchChange({ tab })
            }}
            profile={open}
            tab={search.tab}
          />
        )
      }
    >
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/iam/create-instance-profile">
            <Plus data-icon="inline-start" />
            Create instance profile
          </Link>
        }
        subtitle="Containers that pass a role to an EC2 instance."
        title="Instance profiles"
      />
      <DataTable
        activeId={open?.InstanceProfileName}
        caption="Instance profiles"
        columns={COLUMNS}
        empty="No instance profiles found."
        getRowId={(profile) => profile.InstanceProfileName ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
        }}
        onSortChange={setSort}
        rows={instanceProfiles}
        sort={sort}
      />
    </ResourceView>
  )
}
