import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute("/_auth/iam/(groups)/create-group")({
  beforeLoad: () => {
    throw redirect({ to: "/iam/list-groups", search: { create: true } })
  },
})
