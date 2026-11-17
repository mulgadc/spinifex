import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2VpcPeeringConnectionQueryOptions } from "@/queries/ec2"

import { VpcPeeringConnectionDetails } from "../-components/vpc-peering-connection-details"

export const Route = createFileRoute(
  "/_auth/ec2/(vpc-peering-connections)/describe-vpc-peering-connections/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2VpcPeeringConnectionQueryOptions(params.id),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Peering Connection | Mulga`,
      },
    ],
  }),
  component: VpcPeeringConnectionDetail,
})

// The connection's own page: the side panel's tabs, laid out for the full width.
function VpcPeeringConnectionDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2VpcPeeringConnectionQueryOptions(id))
  const pcx = data.VpcPeeringConnections?.[0]

  if (!pcx?.VpcPeeringConnectionId) {
    return (
      <>
        <BackLink to="/ec2/describe-vpc-peering-connections">
          Back to peering connections
        </BackLink>
        <p className="text-muted-foreground">Peering connection not found.</p>
      </>
    )
  }

  return (
    <VpcPeeringConnectionDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-vpc-peering-connections" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      pcx={pcx}
      tab={tab}
    />
  )
}
