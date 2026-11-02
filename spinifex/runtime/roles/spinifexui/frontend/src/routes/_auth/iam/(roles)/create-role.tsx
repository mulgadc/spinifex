import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute("/_auth/iam/(roles)/create-role")({
  beforeLoad: () => {
    throw redirect({ to: "/iam/list-roles", search: { create: true } })
  },
})
