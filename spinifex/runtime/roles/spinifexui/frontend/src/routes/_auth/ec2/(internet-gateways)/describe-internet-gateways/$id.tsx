import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  ec2InternetGatewayQueryOptions,
  ec2InternetGatewaysQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"

import { InternetGatewayDetails } from "../-components/internet-gateway-details"

export const Route = createFileRoute(
  "/_auth/ec2/(internet-gateways)/describe-internet-gateways/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2InternetGatewayQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2VpcsQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Internet Gateway | Mulga`,
      },
    ],
  }),
  component: InternetGatewayDetail,
})

// The gateway's own page: the side panel's tabs, laid out for the full width.
function InternetGatewayDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2InternetGatewayQueryOptions(id))
  const { data: gatewaysData } = useQuery(ec2InternetGatewaysQueryOptions)
  const igw = data.InternetGateways?.[0]

  if (!igw?.InternetGatewayId) {
    return (
      <>
        <BackLink to="/ec2/describe-internet-gateways">
          Back to internet gateways
        </BackLink>
        <p className="text-muted-foreground">Internet gateway not found.</p>
      </>
    )
  }

  return (
    <InternetGatewayDetails
      gateways={gatewaysData?.InternetGateways ?? [igw]}
      igw={igw}
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-internet-gateways" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
