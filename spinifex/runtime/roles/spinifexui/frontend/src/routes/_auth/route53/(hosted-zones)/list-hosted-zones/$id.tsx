import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  route53HostedZoneQueryOptions,
  route53RecordSetsQueryOptions,
} from "@/queries/route53"

import { HostedZoneDetails } from "../-components/hosted-zone-details"

export const Route = createFileRoute(
  "/_auth/route53/(hosted-zones)/list-hosted-zones/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
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
  component: HostedZoneDetail,
})

// The zone's own page: the side panel's tabs, laid out for the full width.
function HostedZoneDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(route53HostedZoneQueryOptions(id))
  const zone = data.HostedZone

  if (!zone) {
    return (
      <>
        <BackLink to="/route53/list-hosted-zones">
          Back to hosted zones
        </BackLink>
        <p className="text-muted-foreground">Hosted zone not found.</p>
      </>
    )
  }

  return (
    <HostedZoneDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/route53/list-hosted-zones" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
      zone={zone}
    />
  )
}
