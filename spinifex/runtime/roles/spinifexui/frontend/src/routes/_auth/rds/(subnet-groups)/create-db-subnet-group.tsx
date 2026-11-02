import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/rds/(subnet-groups)/create-db-subnet-group",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/rds/describe-db-subnet-groups",
      search: { create: true },
    })
  },
})
