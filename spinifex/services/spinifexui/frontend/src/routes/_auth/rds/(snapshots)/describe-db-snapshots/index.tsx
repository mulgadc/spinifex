import { createFileRoute } from "@tanstack/react-router"

import {
  rdsDBInstancesQueryOptions,
  rdsDBSnapshotsQueryOptions,
} from "@/queries/rds"

import { DBSnapshotsListPage } from "../-components/db-snapshots-list-page"

export const Route = createFileRoute(
  "/_auth/rds/(snapshots)/describe-db-snapshots/",
)({
  loader: async ({ context }) => {
    // The instances are the create dialog's picker, which is mounted with the
    // page rather than on demand.
    await Promise.all([
      context.queryClient.ensureQueryData(rdsDBSnapshotsQueryOptions),
      context.queryClient.ensureQueryData(rdsDBInstancesQueryOptions),
    ])
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
