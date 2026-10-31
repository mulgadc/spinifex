import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/rds/(db-instances)/create-db-instance",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/rds/describe-db-instances",
      search: { create: true },
    })
  },
})
