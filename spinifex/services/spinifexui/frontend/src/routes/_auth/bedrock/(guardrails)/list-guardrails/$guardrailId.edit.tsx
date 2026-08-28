import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"

import { guardrailQueryOptions } from "@/queries/bedrock"
import { guardrailToForm } from "@/types/bedrock"

import { GuardrailFormPage } from "../-components/guardrail-form-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(guardrails)/list-guardrails/$guardrailId/edit",
)({
  loader: async ({ context, params }) => {
    await context.queryClient.ensureQueryData(
      guardrailQueryOptions(params.guardrailId),
    )
  },
  head: ({ params }) => ({
    meta: [{ title: `Edit ${params.guardrailId} | Guardrail | Mulga` }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { guardrailId } = Route.useParams()
  const { data: guardrail } = useSuspenseQuery(
    guardrailQueryOptions(guardrailId),
  )
  return (
    <GuardrailFormPage
      defaultValues={guardrailToForm(guardrail)}
      guardrailId={guardrailId}
      mode="edit"
    />
  )
}
