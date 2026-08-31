import { createFileRoute } from "@tanstack/react-router"

import { foundationModelsQueryOptions } from "@/queries/bedrock"

import { KnowledgeBaseFormPage } from "../-components/knowledge-base-form-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(knowledge-bases)/list-knowledge-bases/create",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(foundationModelsQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Create Knowledge Base | Ochre | Mulga" }],
  }),
  component: KnowledgeBaseFormPage,
})
