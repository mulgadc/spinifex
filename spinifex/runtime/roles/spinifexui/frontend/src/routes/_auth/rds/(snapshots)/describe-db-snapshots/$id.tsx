import { createFileRoute } from "@tanstack/react-router"

import { safeDecodeURIComponent } from "@/lib/utils"
import {
  rdsDBSnapshotQueryOptions,
  rdsSnapshotEventsQueryOptions,
  rdsTagsQueryOptions,
} from "@/queries/rds"

import { DBSnapshotDetailPage } from "../-components/db-snapshot-detail-page"

export const Route = createFileRoute(
  "/_auth/rds/(snapshots)/describe-db-snapshots/$id",
)({
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

function RouteComponent() {
  const { id } = Route.useParams()
  return (
    <DBSnapshotDetailPage dbSnapshotIdentifier={safeDecodeURIComponent(id)} />
  )
}
