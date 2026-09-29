import { createFileRoute } from "@tanstack/react-router"

import {
  guardrailQueryOptions,
  guardrailVersionsQueryOptions,
} from "@/queries/bedrock"

import { GuardrailDetailPage } from "../-components/guardrail-detail-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(guardrails)/list-guardrails/$guardrailId/",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...guardrailQueryOptions(params.guardrailId),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...guardrailVersionsQueryOptions(params.guardrailId),
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.guardrailId} | Guardrail | Mulga` }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { guardrailId } = Route.useParams()
  return <GuardrailDetailPage guardrailId={guardrailId} />
}
