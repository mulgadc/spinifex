import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/ec2/(elastic-ips)/allocate-address",
)({
  beforeLoad: () => {
    throw redirect({ to: "/ec2/describe-addresses", search: { create: true } })
  },
})
