import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/ec2/(vpc-peering-connections)/create-vpc-peering-connection",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/ec2/describe-vpc-peering-connections",
      search: { create: true },
    })
  },
})
