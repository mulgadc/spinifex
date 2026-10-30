import type { KnowledgeBaseSummary } from "@aws-sdk/client-bedrock-agent"
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
import { formatDateTime } from "@/lib/utils"
import { useDeleteKnowledgeBase } from "@/mutations/bedrockAgent"

import { KnowledgeBaseDetails } from "./knowledge-base-details"
import { KnowledgeBaseFormPage } from "./knowledge-base-form-page"

const COLUMNS: DataColumn<KnowledgeBaseSummary>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (kb) => kb.name,
    cell: (kb) => (
      <Link
        className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/bedrock/list-knowledge-bases/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: kb.knowledgeBaseId ?? "",
        })}
        to="."
      >
        <span className="block truncate font-medium text-primary group-hover:underline">
          {kb.name ?? kb.knowledgeBaseId}
        </span>
      </Link>
    ),
  },
  {
    id: "status",
    header: "Status",
    sortValue: (kb) => kb.status,
    cell: (kb) => <StateBadge state={kb.status} />,
  },
  {
    id: "id",
    header: "Knowledge base ID",
    sortValue: (kb) => kb.knowledgeBaseId,
    hideBelow: "md",
    cell: (kb) => (
      <span className="font-mono text-xs">{kb.knowledgeBaseId}</span>
    ),
  },
  {
    id: "updated",
    header: "Updated",
    sortValue: (kb) => kb.updatedAt?.getTime(),
    hideBelow: "lg",
    cell: (kb) => formatDateTime(kb.updatedAt),
  },
]

interface KnowledgeBasesPageProps {
  knowledgeBases: readonly KnowledgeBaseSummary[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function KnowledgeBasesPage({
  knowledgeBases,
  search,
  onSearchChange,
}: KnowledgeBasesPageProps) {
  const [sort, setSort] = useState<SortState>({ column: "name", desc: false })

  const open = knowledgeBases.find(
    (kb) => search.selected !== "" && kb.knowledgeBaseId === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const selection = useRowSelection(
    knowledgeBases.map((kb) => kb.knowledgeBaseId ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteKnowledgeBase = useDeleteKnowledgeBase()

  const names = new Map(
    knowledgeBases.map((kb) => [kb.knowledgeBaseId, kb.name]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel onClose={closePanel} title="Create knowledge base">
        <KnowledgeBaseFormPage
          onClose={(knowledgeBaseId) => {
            onSearchChange({ ...CLOSED_PANEL, selected: knowledgeBaseId ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.knowledgeBaseId) {
    panel = (
      <KnowledgeBaseDetails
        key={open.knowledgeBaseId}
        knowledgeBase={open}
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
            from="/bedrock/list-knowledge-bases/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create knowledge base
          </Link>
        }
        title="Knowledge bases"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteKnowledgeBase.mutateAsync}
            description="This tears them down with all of their data sources. It cannot be undone."
            ids={selection.ids}
            label={(id) => `${names.get(id) ?? id} (${id})`}
            noun={["knowledge base", "knowledge bases"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.knowledgeBaseId}
        caption="Knowledge bases"
        columns={COLUMNS}
        empty="No knowledge bases found."
        getRowId={(kb) => kb.knowledgeBaseId ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={knowledgeBases}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
