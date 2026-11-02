import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2NatGatewayQueryOptions } from "@/queries/ec2"

import { NatGatewayDetails } from "../-components/nat-gateway-details"

export const Route = createFileRoute(
  "/_auth/ec2/(nat-gateways)/describe-nat-gateways/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2NatGatewayQueryOptions(params.id),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | NAT Gateway | Mulga`,
      },
    ],
  }),
  component: NatGatewayDetail,
})

// The NAT gateway's own page: the side panel's tabs, laid out for the full width.
function NatGatewayDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2NatGatewayQueryOptions(id))
  const nat = data.NatGateways?.[0]

  if (!nat?.NatGatewayId) {
    return (
      <>
        <BackLink to="/ec2/describe-nat-gateways">
          Back to NAT gateways
        </BackLink>
        <p className="text-muted-foreground">NAT gateway not found.</p>
      </>
    )
  }

  return (
    <NatGatewayDetails
      layout="page"
      nat={nat}
      onClose={() => {
        void navigate({ to: "/ec2/describe-nat-gateways" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
