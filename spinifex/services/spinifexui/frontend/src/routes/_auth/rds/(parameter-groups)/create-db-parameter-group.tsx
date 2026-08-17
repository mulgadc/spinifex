import { createFileRoute } from "@tanstack/react-router"

import { rdsEngineVersionsQueryOptions } from "@/queries/rds"

import { CreateDBParameterGroupPage } from "./-components/create-db-parameter-group-page"

export const Route = createFileRoute(
  "/_auth/rds/(parameter-groups)/create-db-parameter-group",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(rdsEngineVersionsQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Create parameter group | RDS | Mulga",
      },
    ],
  }),
  component: CreateDBParameterGroupPage,
})
