import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/rds/(parameter-groups)/create-db-parameter-group",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/rds/describe-db-parameter-groups",
      search: { create: true },
    })
  },
})
