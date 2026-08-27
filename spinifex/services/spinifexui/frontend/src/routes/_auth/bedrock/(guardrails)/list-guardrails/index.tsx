import { createFileRoute } from "@tanstack/react-router"

import { guardrailsQueryOptions } from "@/queries/bedrock"

import { GuardrailsPage } from "../-components/guardrails-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(guardrails)/list-guardrails/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(guardrailsQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Guardrails | Ochre | Mulga" }],
  }),
  component: GuardrailsPage,
})
