import { createFileRoute } from "@tanstack/react-router"

import { ecsClustersQueryOptions } from "@/queries/ecs"

import { ClustersListPage } from "../-components/clusters-list-page"

export const Route = createFileRoute("/_auth/ecs/(clusters)/list-clusters/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ecsClustersQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Clusters | ECS | Mulga" }],
  }),
  component: ClustersListPage,
})
