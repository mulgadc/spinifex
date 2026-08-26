import { createFileRoute } from "@tanstack/react-router"

import { knowledgeBasesQueryOptions } from "@/queries/bedrockAgent"

import { KnowledgeBasesPage } from "../-components/knowledge-bases-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(knowledge-bases)/list-knowledge-bases/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(knowledgeBasesQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Knowledge Bases | Ochre | Mulga" }],
  }),
  component: KnowledgeBasesPage,
})
