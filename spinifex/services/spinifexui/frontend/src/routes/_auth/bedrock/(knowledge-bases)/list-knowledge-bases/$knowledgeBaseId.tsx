import { createFileRoute } from "@tanstack/react-router"

import {
  dataSourcesQueryOptions,
  knowledgeBaseQueryOptions,
} from "@/queries/bedrockAgent"

import { KnowledgeBaseDetailPage } from "../-components/knowledge-base-detail-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(knowledge-bases)/list-knowledge-bases/$knowledgeBaseId",
)({
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
  component: RouteComponent,
})

function RouteComponent() {
  const { knowledgeBaseId } = Route.useParams()
  return <KnowledgeBaseDetailPage knowledgeBaseId={knowledgeBaseId} />
}
