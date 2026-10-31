import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { knowledgeBasesQueryOptions } from "@/queries/bedrockAgent"

import { KnowledgeBasesPage } from "../-components/knowledge-bases-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(knowledge-bases)/list-knowledge-bases/",
)({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...knowledgeBasesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Knowledge Bases | Ochre | Mulga" }],
  }),
  component: KnowledgeBases,
})

function KnowledgeBases() {
  const { data } = useSuspenseQuery(knowledgeBasesQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return (
    <KnowledgeBasesPage
      knowledgeBases={data.knowledgeBaseSummaries ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
