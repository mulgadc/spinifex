import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute("/_auth/iam/(policies)/create-policy")({
  beforeLoad: () => {
    throw redirect({ to: "/iam/list-policies", search: { create: true } })
  },
})
