import { createFileRoute } from "@tanstack/react-router"

import { route53HostedZonesQueryOptions } from "@/queries/route53"

import { ListHostedZonesPage } from "../-components/list-hosted-zones-page"

export const Route = createFileRoute(
  "/_auth/route53/(hosted-zones)/list-hosted-zones/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...route53HostedZonesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Hosted zones | DNS | Mulga",
      },
    ],
  }),
  component: ListHostedZonesPage,
})
