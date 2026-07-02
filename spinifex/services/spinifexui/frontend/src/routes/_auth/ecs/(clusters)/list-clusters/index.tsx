import { createFileRoute } from "@tanstack/react-router"

import { ec2ImagesQueryOptions } from "@/queries/ec2"
import { ecsClustersQueryOptions } from "@/queries/ecs"

import { ClustersListPage } from "../-components/clusters-list-page"

export const Route = createFileRoute("/_auth/ecs/(clusters)/list-clusters/")({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ecsClustersQueryOptions),
      context.queryClient.ensureQueryData(ec2ImagesQueryOptions),
    ])
  },
  head: () => ({
    meta: [{ title: "Clusters | ECS | Mulga" }],
  }),
  component: ClustersListPage,
})
