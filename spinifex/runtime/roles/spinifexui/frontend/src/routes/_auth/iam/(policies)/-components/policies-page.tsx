import type { Policy } from "@aws-sdk/client-iam"
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
import { useDeletePolicy } from "@/mutations/iam"

import { iamDeleteWarning } from "../../-components/delete-prerequisites"
import { CreatePolicyPage } from "./create-policy-page"
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

  const selection = useRowSelection(
    policies.map((policy) => policy.Arn ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const remove = useDeletePolicy()

  const names = new Map(
    policies.map((policy) => [policy.Arn, policy.PolicyName]),
  )

  return (
    <ResourceView
      panel={
        search.create ? (
          <CreatePanel
            onClose={() => {
              onSearchChange(CLOSED_PANEL)
            }}
            title="Create policy"
          >
            <CreatePolicyPage
              onClose={(id) => {
                onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
              }}
            />
          </CreatePanel>
        ) : (
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
        )
      }
    >
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/iam/list-policies/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create policy
          </Link>
        }
        subtitle="Managed policies: permissions you attach to users, groups and roles."
        title="Policies"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={remove.mutateAsync}
            description={iamDeleteWarning("policy", selection.ids.length)}
            ids={selection.ids}
            label={(id) => `${names.get(id) ?? id} (${id})`}
            noun={["policy", "policies"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.Arn}
        caption="Policies"
        columns={COLUMNS}
        empty="No IAM policies found."
        getRowId={(policy) => policy.Arn ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={policies}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
