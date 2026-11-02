import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { panelSearchFields } from "@/lib/panel-search"
import {
  guardrailQueryOptions,
  guardrailVersionsQueryOptions,
} from "@/queries/bedrock"

import { GuardrailDetails } from "../-components/guardrail-details"

export const Route = createFileRoute(
  "/_auth/bedrock/(guardrails)/list-guardrails/$guardrailId/",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
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
  component: GuardrailDetail,
})

// The guardrail's own page: the side panel's tabs, laid out for the full width.
function GuardrailDetail() {
  const { guardrailId } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(guardrailQueryOptions(guardrailId))

  return (
    <GuardrailDetails
      guardrail={{ id: guardrailId, name: data.name, status: data.status }}
      layout="page"
      onClose={() => {
        void navigate({ to: "/bedrock/list-guardrails" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
