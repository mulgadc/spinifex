import { createFileRoute } from "@tanstack/react-router"

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
    const id = decodeURIComponent(params.id)
    // The tags query keys off the ARN, which only the describe knows, so it is
    // warmed after the snapshot rather than alongside it.
    const [snapshot] = await Promise.all([
      context.queryClient.ensureQueryData(rdsDBSnapshotQueryOptions(id)),
      context.queryClient.ensureQueryData(rdsSnapshotEventsQueryOptions(id)),
    ])
    const arn = snapshot.DBSnapshots?.[0]?.DBSnapshotArn ?? ""
    if (arn !== "") {
      await context.queryClient.ensureQueryData(rdsTagsQueryOptions(arn))
    }
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${decodeURIComponent(params.id)} | RDS | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { id } = Route.useParams()
  return <DBSnapshotDetailPage dbSnapshotIdentifier={decodeURIComponent(id)} />
}
