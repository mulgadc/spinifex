import type { KnowledgeBaseSummary } from "@aws-sdk/client-bedrock-agent"
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
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"

import { KnowledgeBaseDetails } from "./knowledge-base-details"

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

  return (
    <ResourceView
      panel={
        open && (
          <KnowledgeBaseDetails
            knowledgeBase={open}
            key={open.knowledgeBaseId}
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
    >
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            to="/bedrock/list-knowledge-bases/create"
          >
            <Plus data-icon="inline-start" />
            Create knowledge base
          </Link>
        }
        title="Knowledge bases"
      />
      <DataTable
        activeId={open?.knowledgeBaseId}
        caption="Knowledge bases"
        columns={COLUMNS}
        empty="No knowledge bases found."
        getRowId={(kb) => kb.knowledgeBaseId ?? ""}
        onRowClick={(selected) => {
          onSearchChange({ ...CLOSED_PANEL, selected })
        }}
        onSortChange={setSort}
        rows={knowledgeBases}
        sort={sort}
      />
    </ResourceView>
  )
}
