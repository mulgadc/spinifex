import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  ec2InstancesQueryOptions,
  ec2PlacementGroupQueryOptions,
} from "@/queries/ec2"

import { PlacementGroupDetails } from "../-components/placement-group-details"

export const Route = createFileRoute(
  "/_auth/ec2/(placement-groups)/describe-placement-groups/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2PlacementGroupQueryOptions(params.id),
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
        title: `${params.id} | EC2 | Mulga`,
      },
    ],
  }),
  component: PlacementGroupDetail,
})

// The group's own page: the side panel's tabs, laid out for the full width.
function PlacementGroupDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2PlacementGroupQueryOptions(id))
  const pg = data.PlacementGroups?.[0]

  if (!pg?.GroupId) {
    return (
      <>
        <BackLink to="/ec2/describe-placement-groups">
          Back to placement groups
        </BackLink>
        <p className="text-muted-foreground">Placement group not found.</p>
      </>
    )
  }

  return (
    <PlacementGroupDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-placement-groups" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      pg={pg}
      tab={tab}
    />
  )
}
