import { createFileRoute } from "@tanstack/react-router"

import {
  ecsClusterQueryOptions,
  ecsServicesQueryOptions,
  ecsTasksQueryOptions,
} from "@/queries/ecs"

import { ClusterDetailPage } from "../-components/cluster-detail-page"

export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/list-clusters/$clusterName",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ecsClusterQueryOptions(params.clusterName),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ecsServicesQueryOptions(params.clusterName),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ecsTasksQueryOptions(params.clusterName),
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.clusterName} | ECS | Mulga` }],
  }),
  component: ClusterDetail,
})

function ClusterDetail() {
  const { clusterName } = Route.useParams()
  return <ClusterDetailPage clusterName={clusterName} />
}
