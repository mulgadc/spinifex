import { createFileRoute } from "@tanstack/react-router"

import { rdsParameterGroupsQueryOptions } from "@/queries/rds"

import { DBParameterGroupsListPage } from "../-components/db-parameter-groups-list-page"

export const Route = createFileRoute(
  "/_auth/rds/(parameter-groups)/describe-db-parameter-groups/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(rdsParameterGroupsQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Parameter groups | RDS | Mulga",
      },
    ],
  }),
  component: DBParameterGroupsListPage,
})
