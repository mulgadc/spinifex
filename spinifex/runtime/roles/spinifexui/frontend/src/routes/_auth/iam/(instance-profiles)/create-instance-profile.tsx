import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/iam/(instance-profiles)/create-instance-profile",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/iam/list-instance-profiles",
      search: { create: true },
    })
  },
})
