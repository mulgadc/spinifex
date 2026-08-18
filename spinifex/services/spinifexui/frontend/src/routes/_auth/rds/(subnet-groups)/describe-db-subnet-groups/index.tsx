import { createFileRoute } from "@tanstack/react-router"

import { rdsSubnetGroupsQueryOptions } from "@/queries/rds"

import { DBSubnetGroupsListPage } from "../-components/db-subnet-groups-list-page"

export const Route = createFileRoute(
  "/_auth/rds/(subnet-groups)/describe-db-subnet-groups/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(rdsSubnetGroupsQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Subnet Groups | RDS | Mulga",
      },
    ],
  }),
  component: DBSubnetGroupsListPage,
})
