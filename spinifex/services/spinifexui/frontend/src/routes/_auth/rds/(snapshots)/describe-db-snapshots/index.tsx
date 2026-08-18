import { createFileRoute } from "@tanstack/react-router"

import { rdsDBSnapshotsQueryOptions } from "@/queries/rds"

import { DBSnapshotsListPage } from "../-components/db-snapshots-list-page"

export const Route = createFileRoute(
  "/_auth/rds/(snapshots)/describe-db-snapshots/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(rdsDBSnapshotsQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Snapshots | RDS | Mulga",
      },
    ],
  }),
  component: DBSnapshotsListPage,
})
