import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  ec2InternetGatewaysQueryOptions,
  ec2NatGatewaysQueryOptions,
  ec2RouteTableQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"

import { RouteTableDetails } from "../-components/route-table-details"

export const Route = createFileRoute(
  "/_auth/ec2/(route-tables)/describe-route-tables/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2RouteTableQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2InternetGatewaysQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2NatGatewaysQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2SubnetsQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Route Table | Mulga`,
      },
    ],
  }),
  component: RouteTableDetail,
})

// The route table's own page: the side panel's tabs, laid out for the full width.
function RouteTableDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2RouteTableQueryOptions(id))
  const rtb = data.RouteTables?.[0]

  if (!rtb?.RouteTableId) {
    return (
      <>
        <BackLink to="/ec2/describe-route-tables">
          Back to route tables
        </BackLink>
        <p className="text-muted-foreground">Route table not found.</p>
      </>
    )
  }

  return (
    <RouteTableDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-route-tables" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      rtb={rtb}
      tab={tab}
    />
  )
}
