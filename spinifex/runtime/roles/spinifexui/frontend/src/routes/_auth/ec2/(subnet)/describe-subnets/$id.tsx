import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2SubnetQueryOptions } from "@/queries/ec2"

import { SubnetDetails } from "../-components/subnet-details"

export const Route = createFileRoute(
  "/_auth/ec2/(subnet)/describe-subnets/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2SubnetQueryOptions(params.id),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Subnet | Mulga`,
      },
    ],
  }),
  component: SubnetDetail,
})

// The subnet's own page: the side panel's tabs, laid out for the full width.
function SubnetDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2SubnetQueryOptions(id))
  const subnet = data.Subnets?.[0]

  if (!subnet?.SubnetId) {
    return (
      <>
        <BackLink to="/ec2/describe-subnets">Back to subnets</BackLink>
        <p className="text-muted-foreground">Subnet not found.</p>
      </>
    )
  }

  return (
    <SubnetDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-subnets" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      subnet={subnet}
      tab={tab}
    />
  )
}
