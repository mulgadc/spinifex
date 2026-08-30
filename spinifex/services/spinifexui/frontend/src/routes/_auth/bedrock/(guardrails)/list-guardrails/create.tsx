import { createFileRoute } from "@tanstack/react-router"

import { EMPTY_GUARDRAIL_FORM_DEFAULTS } from "@/types/bedrock"

import { GuardrailFormPage } from "../-components/guardrail-form-page"

export const Route = createFileRoute(
  "/_auth/bedrock/(guardrails)/list-guardrails/create",
)({
  head: () => ({
    meta: [{ title: "Create Guardrail | Ochre | Mulga" }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  return (
    <GuardrailFormPage
      defaultValues={EMPTY_GUARDRAIL_FORM_DEFAULTS}
      mode="create"
    />
  )
}
