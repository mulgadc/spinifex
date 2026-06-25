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
      context.queryClient.ensureQueryData(
        ecsClusterQueryOptions(params.clusterName),
      ),
      context.queryClient.ensureQueryData(
        ecsServicesQueryOptions(params.clusterName),
      ),
      context.queryClient.ensureQueryData(
        ecsTasksQueryOptions(params.clusterName),
      ),
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
