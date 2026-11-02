import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/ec2/(load-balancers)/create-load-balancer",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/ec2/describe-load-balancers",
      search: { create: true },
    })
  },
})
