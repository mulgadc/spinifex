import { createFileRoute } from "@tanstack/react-router"

import { rdsDBInstancesQueryOptions } from "@/queries/rds"

import { DBInstancesListPage } from "../-components/db-instances-list-page"

export const Route = createFileRoute(
  "/_auth/rds/(db-instances)/describe-db-instances/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...rdsDBInstancesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Databases | RDS | Mulga",
      },
    ],
  }),
  component: DBInstancesListPage,
})
