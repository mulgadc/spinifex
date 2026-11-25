import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { pageTitle } from "@/lib/branding"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2TransitGatewayQueryOptions } from "@/queries/ec2"

import { TransitGatewayDetails } from "../-components/transit-gateway-details"

export const Route = createFileRoute(
  "/_auth/ec2/(transit-gateways)/describe-transit-gateways/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2TransitGatewayQueryOptions(params.id),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: pageTitle(`${params.id} | Transit Gateway`),
      },
    ],
  }),
  component: TransitGatewayDetail,
})

// The transit gateway's own page: the side panel's tabs, laid out for the full width.
function TransitGatewayDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2TransitGatewayQueryOptions(id))
  const tgw = data.TransitGateways?.[0]

  if (!tgw?.TransitGatewayId) {
    return (
      <>
        <BackLink to="/ec2/describe-transit-gateways">
          Back to transit gateways
        </BackLink>
        <p className="text-muted-foreground">Transit gateway not found.</p>
      </>
    )
  }

  return (
    <TransitGatewayDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-transit-gateways" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
      tgw={tgw}
    />
  )
}
