import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute(
  "/_auth/route53/(hosted-zones)/create-hosted-zone",
)({
  beforeLoad: () => {
    throw redirect({
      to: "/route53/list-hosted-zones",
      search: { create: true },
    })
  },
})
