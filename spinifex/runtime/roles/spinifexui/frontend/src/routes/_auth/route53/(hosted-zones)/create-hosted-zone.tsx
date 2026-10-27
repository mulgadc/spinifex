import { createFileRoute } from "@tanstack/react-router"

import { CreateHostedZonePage } from "./-components/create-hosted-zone-page"

export const Route = createFileRoute(
  "/_auth/route53/(hosted-zones)/create-hosted-zone",
)({
  head: () => ({
    meta: [
      {
        title: "Add domain | DNS | Mulga",
      },
    ],
  }),
  component: CreateHostedZonePage,
})
