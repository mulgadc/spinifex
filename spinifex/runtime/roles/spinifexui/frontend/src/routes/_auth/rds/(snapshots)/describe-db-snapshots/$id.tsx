import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { safeDecodeURIComponent } from "@/lib/utils"
import {
  rdsDBSnapshotQueryOptions,
  rdsSnapshotEventsQueryOptions,
  rdsTagsQueryOptions,
} from "@/queries/rds"

import { DBSnapshotDetails } from "../-components/db-snapshot-details"

export const Route = createFileRoute(
  "/_auth/rds/(snapshots)/describe-db-snapshots/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    const id = safeDecodeURIComponent(params.id)

    // The tags query keys off the ARN, which only the describe knows, so it is
    // warmed after the snapshot rather than alongside it.
    const [snapshot] = await Promise.all([
      context.queryClient.query({
        ...rdsDBSnapshotQueryOptions(id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...rdsSnapshotEventsQueryOptions(id),
        staleTime: "static",
      }),
    ])

    const arn = snapshot.DBSnapshots?.[0]?.DBSnapshotArn ?? ""

    if (arn !== "") {
      await context.queryClient.query({
        ...rdsTagsQueryOptions(arn),
        staleTime: "static",
      })
    }
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${safeDecodeURIComponent(params.id)} | RDS | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

// The snapshot's own page: the side panel's tabs, laid out for the full width.
function RouteComponent() {
  const id = safeDecodeURIComponent(Route.useParams().id)
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(rdsDBSnapshotQueryOptions(id))
  const snapshot = data.DBSnapshots?.[0]

  if (!snapshot?.DBSnapshotIdentifier) {
    return (
      <>
        <BackLink to="/rds/describe-db-snapshots">Back to snapshots</BackLink>
        <p className="text-muted-foreground">DB snapshot not found.</p>
      </>
    )
  }

  return (
    <DBSnapshotDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/rds/describe-db-snapshots" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      snapshot={snapshot}
      tab={tab}
    />
  )
}
