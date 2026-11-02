import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2RegionsQueryOptions, ec2SnapshotQueryOptions } from "@/queries/ec2"

import { SnapshotDetails } from "../-components/snapshot-details"

export const Route = createFileRoute(
  "/_auth/ec2/(snapshots)/describe-snapshots/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2SnapshotQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2RegionsQueryOptions,
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
  component: SnapshotDetail,
})

// The snapshot's own page: the side panel's tabs, laid out for the full width.
function SnapshotDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2SnapshotQueryOptions(id))
  const snapshot = data.Snapshots?.[0]

  if (!snapshot?.SnapshotId) {
    return (
      <>
        <BackLink to="/ec2/describe-snapshots">Back to snapshots</BackLink>
        <p className="text-muted-foreground">Snapshot not found.</p>
      </>
    )
  }

  return (
    <SnapshotDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-snapshots" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      snapshot={snapshot}
      tab={tab}
    />
  )
}
