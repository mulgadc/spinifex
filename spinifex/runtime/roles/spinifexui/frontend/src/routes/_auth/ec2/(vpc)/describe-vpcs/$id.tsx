import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2SubnetsQueryOptions, ec2VpcQueryOptions } from "@/queries/ec2"

import { VpcDetails } from "../-components/vpc-details"

export const Route = createFileRoute("/_auth/ec2/(vpc)/describe-vpcs/$id")({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2VpcQueryOptions(params.id),
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
        title: `${params.id} | VPC | Mulga`,
      },
    ],
  }),
  component: VpcDetail,
})

// The VPC's own page: the side panel's tabs, laid out for the full width.
function VpcDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2VpcQueryOptions(id))
  const vpc = data.Vpcs?.[0]

  if (!vpc?.VpcId) {
    return (
      <>
        <BackLink to="/ec2/describe-vpcs">Back to VPCs</BackLink>
        <p className="text-muted-foreground">VPC not found.</p>
      </>
    )
  }

  return (
    <VpcDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-vpcs" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
      vpc={vpc}
    />
  )
}
