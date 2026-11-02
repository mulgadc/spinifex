import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute("/_auth/eks/(clusters)/create-cluster")({
  beforeLoad: () => {
    throw redirect({ to: "/eks/list-clusters", search: { create: true } })
  },
})
