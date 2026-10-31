import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2InstancesQueryOptions, ec2VolumeQueryOptions } from "@/queries/ec2"

import { VolumeDetails } from "../-components/volume-details"

export const Route = createFileRoute(
  "/_auth/ec2/(volumes)/describe-volumes/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2VolumeQueryOptions(params.id),
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
  component: VolumeDetail,
})

// The volume's own page: the side panel's tabs, laid out for the full width.
function VolumeDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2VolumeQueryOptions(id))
  const volume = data.Volumes?.[0]

  if (!volume?.VolumeId) {
    return (
      <>
        <BackLink to="/ec2/describe-volumes">Back to volumes</BackLink>
        <p className="text-muted-foreground">Volume not found.</p>
      </>
    )
  }

  return (
    <VolumeDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-volumes" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
      volume={volume}
    />
  )
}
