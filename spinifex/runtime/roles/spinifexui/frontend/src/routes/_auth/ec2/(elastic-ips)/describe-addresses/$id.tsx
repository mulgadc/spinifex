import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2AddressQueryOptions, ec2InstancesQueryOptions } from "@/queries/ec2"

import { ElasticIpDetails } from "../-components/elastic-ip-details"

export const Route = createFileRoute(
  "/_auth/ec2/(elastic-ips)/describe-addresses/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2AddressQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2InstancesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Elastic IP | Mulga`,
      },
    ],
  }),
  component: AddressDetail,
})

// The address's own page: the side panel's tabs, laid out for the full width.
function AddressDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2AddressQueryOptions(id))
  const address = data.Addresses?.[0]

  if (!address?.AllocationId) {
    return (
      <>
        <BackLink to="/ec2/describe-addresses">Back to Elastic IPs</BackLink>
        <p className="text-muted-foreground">Elastic IP not found.</p>
      </>
    )
  }

  return (
    <ElasticIpDetails
      address={address}
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-addresses" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
