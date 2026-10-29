import type { GuardrailSummary } from "@aws-sdk/client-bedrock"
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
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteGuardrail } from "@/mutations/bedrock"

import { GuardrailDetails } from "./guardrail-details"

const COLUMNS: DataColumn<GuardrailSummary>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (guardrail) => guardrail.name,
    cell: (guardrail) => (
      <Link
        className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/bedrock/list-guardrails/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: guardrail.id ?? "",
        })}
        to="."
      >
        <span className="block truncate font-medium text-primary group-hover:underline">
          {guardrail.name ?? guardrail.id}
        </span>
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    sortValue: (guardrail) => guardrail.status,
    cell: (guardrail) => <StateBadge state={guardrail.status} />,
  },
  {
    id: "id",
    header: "Guardrail ID",
    sortValue: (guardrail) => guardrail.id,
    hideBelow: "md",
    cell: (guardrail) => (
      <span className="font-mono text-xs">{guardrail.id}</span>
    ),
  },
  {
    id: "updated",
    header: "Updated",
    sortValue: (guardrail) => guardrail.updatedAt?.getTime(),
    hideBelow: "lg",
    cell: (guardrail) => formatDateTime(guardrail.updatedAt),
  },
]

interface GuardrailsPageProps {
  guardrails: readonly GuardrailSummary[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function GuardrailsPage({
  guardrails,
  search,
  onSearchChange,
}: GuardrailsPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = guardrails.find(
    (guardrail) => search.selected !== "" && guardrail.id === search.selected,
  )

  const selection = useRowSelection(
    guardrails.map((guardrail) => guardrail.id ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteGuardrail = useDeleteGuardrail()

  const names = new Map(
    guardrails.map((guardrail) => [guardrail.id, guardrail.name]),
  )

  let panel: ReactNode = null

  if (open?.id) {
    panel = (
      <GuardrailDetails
        guardrail={open}
        key={open.id}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
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
            to="/bedrock/list-guardrails/create"
          >
            <Plus data-icon="inline-start" />
            Create guardrail
          </Link>
        }
        title="Guardrails"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteGuardrail.mutateAsync}
            description="This tears them down with all of their versions. It cannot be undone."
            ids={selection.ids}
            label={(id) => `${names.get(id) ?? id} (${id})`}
            noun={["guardrail", "guardrails"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.id}
        caption="Guardrails"
        columns={COLUMNS}
        empty="No guardrails found."
        getRowId={(guardrail) => guardrail.id ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={guardrails}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
