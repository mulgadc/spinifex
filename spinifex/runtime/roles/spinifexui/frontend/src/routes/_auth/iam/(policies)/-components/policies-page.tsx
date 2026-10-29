import type { Policy } from "@aws-sdk/client-iam"
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

import { PolicyDetails } from "./policy-details"

const COLUMNS: DataColumn<Policy>[] = [
  {
    id: "name",
    header: "Policy name",
    sortValue: (policy) => policy.PolicyName,
    cell: (policy) => (
      <Link
        className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/iam/list-policies/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: policy.Arn ?? "",
        })}
        to="."
      >
        <span className="block truncate font-medium text-primary group-hover:underline">
          {policy.PolicyName}
        </span>
        <span className="block truncate font-mono text-xs text-muted-foreground">
          {policy.Arn}
        </span>
      </Link>
    ),
  },
  {
    id: "attachments",
    header: "Attachments",
    sortValue: (policy) => policy.AttachmentCount,
    hideBelow: "md",
    align: "end",
    cell: (policy) => (
      <span className="tabular-nums">{policy.AttachmentCount ?? 0}</span>
    ),
  },
  {
    id: "updated",
    header: "Updated",
    sortValue: (policy) => policy.UpdateDate?.getTime(),
    hideBelow: "lg",
    cell: (policy) => formatDateTime(policy.UpdateDate),
  },
]

interface PoliciesPageProps {
  policies: readonly Policy[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function PoliciesPage({
  policies,
  search,
  onSearchChange,
}: PoliciesPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = policies.find(
    (policy) => search.selected !== "" && policy.Arn === search.selected,
  )

  return (
    <ResourceView
      panel={
        open && (
          <PolicyDetails
            key={open.Arn}
            onClose={() => {
              onSearchChange(CLOSED_PANEL)
            }}
            onTabChange={(tab) => {
              onSearchChange({ tab })
            }}
            policy={open}
            tab={search.tab}
          />
        )
      }
    >
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/iam/create-policy">
            <Plus data-icon="inline-start" />
            Create policy
          </Link>
        }
        subtitle="Managed policies: permissions you attach to users, groups and roles."
        title="Policies"
      />
      <DataTable
        activeId={open?.Arn}
        caption="Policies"
        columns={COLUMNS}
        empty="No IAM policies found."
        getRowId={(policy) => policy.Arn ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
        }}
        onSortChange={setSort}
        rows={policies}
        sort={sort}
      />
    </ResourceView>
  )
}
