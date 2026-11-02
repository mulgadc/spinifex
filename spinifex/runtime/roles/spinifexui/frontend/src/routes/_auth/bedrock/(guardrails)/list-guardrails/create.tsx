import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/bedrock/(guardrails)/list-guardrails/create",
)({
  beforeLoad: () => {
    throw redirect({ to: "/bedrock/list-guardrails", search: { create: true } })
  },
})
