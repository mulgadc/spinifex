import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/ec2/(transit-gateway-attachments)/create-transit-gateway-vpc-attachment",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/ec2/describe-transit-gateway-vpc-attachments",
      search: { create: true },
    })
  },
})
