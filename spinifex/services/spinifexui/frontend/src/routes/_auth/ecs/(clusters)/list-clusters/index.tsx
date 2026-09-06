import { createFileRoute } from "@tanstack/react-router"

import { ec2ImagesQueryOptions } from "@/queries/ec2"
import { ecsClustersQueryOptions } from "@/queries/ecs"

import { ClustersListPage } from "../-components/clusters-list-page"

export const Route = createFileRoute("/_auth/ecs/(clusters)/list-clusters/")({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.query({
        ...ecsClustersQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2ImagesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: () => ({
    meta: [{ title: "Clusters | ECS | Mulga" }],
  }),
  component: ClustersListPage,
})
