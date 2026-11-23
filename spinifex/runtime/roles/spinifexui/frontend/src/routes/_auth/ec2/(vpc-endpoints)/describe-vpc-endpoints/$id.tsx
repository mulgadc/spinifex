import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2VpcEndpointQueryOptions } from "@/queries/ec2"

import { VpcEndpointDetails } from "../-components/vpc-endpoint-details"

export const Route = createFileRoute(
  "/_auth/ec2/(vpc-endpoints)/describe-vpc-endpoints/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2VpcEndpointQueryOptions(params.id),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Endpoint | Mulga`,
      },
    ],
  }),
  component: VpcEndpointDetail,
})

// The endpoint's own page: the side panel's tabs, laid out for the full width.
function VpcEndpointDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2VpcEndpointQueryOptions(id))
  const endpoint = data.VpcEndpoints?.[0]

  if (!endpoint?.VpcEndpointId) {
    return (
      <>
        <BackLink to="/ec2/describe-vpc-endpoints">Back to endpoints</BackLink>
        <p className="text-muted-foreground">Endpoint not found.</p>
      </>
    )
  }

  return (
    <VpcEndpointDetails
      endpoint={endpoint}
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-vpc-endpoints" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
