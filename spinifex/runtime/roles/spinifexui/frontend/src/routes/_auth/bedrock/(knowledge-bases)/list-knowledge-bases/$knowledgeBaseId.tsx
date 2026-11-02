import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  dataSourcesQueryOptions,
  knowledgeBaseQueryOptions,
} from "@/queries/bedrockAgent"

import { KnowledgeBaseDetails } from "../-components/knowledge-base-details"

export const Route = createFileRoute(
  "/_auth/bedrock/(knowledge-bases)/list-knowledge-bases/$knowledgeBaseId",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...knowledgeBaseQueryOptions(params.knowledgeBaseId),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...dataSourcesQueryOptions(params.knowledgeBaseId),
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.knowledgeBaseId} | Knowledge Base | Mulga` }],
  }),
  component: KnowledgeBaseDetail,
})

// The knowledge base's own page: the side panel's tabs, at full width.
function KnowledgeBaseDetail() {
  const { knowledgeBaseId } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(knowledgeBaseQueryOptions(knowledgeBaseId))
  const { knowledgeBase } = data

  if (!knowledgeBase) {
    return (
      <>
        <BackLink to="/bedrock/list-knowledge-bases">
          Back to knowledge bases
        </BackLink>
        <p className="text-muted-foreground">Knowledge base not found.</p>
      </>
    )
  }

  return (
    <KnowledgeBaseDetails
      knowledgeBase={{
        knowledgeBaseId,
        name: knowledgeBase.name,
        status: knowledgeBase.status,
      }}
      layout="page"
      onClose={() => {
        void navigate({ to: "/bedrock/list-knowledge-bases" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
