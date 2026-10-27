import { createFileRoute } from "@tanstack/react-router"

import {
  route53HostedZoneQueryOptions,
  route53RecordSetsQueryOptions,
} from "@/queries/route53"

import { HostedZoneDetailPage } from "../-components/hosted-zone-detail-page"

export const Route = createFileRoute(
  "/_auth/route53/(hosted-zones)/list-hosted-zones/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...route53HostedZoneQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...route53RecordSetsQueryOptions(params.id),
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Hosted zone | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { id } = Route.useParams()

  return <HostedZoneDetailPage zoneId={id} />
}
