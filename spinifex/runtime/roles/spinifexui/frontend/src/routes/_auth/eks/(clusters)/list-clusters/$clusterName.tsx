import { createFileRoute } from "@tanstack/react-router"

import {
  eksAccessEntriesQueryOptions,
  eksClusterQueryOptions,
  eksNodegroupsQueryOptions,
} from "@/queries/eks"

import { ClusterDetailPage } from "../-components/cluster-detail-page"

export const Route = createFileRoute(
  "/_auth/eks/(clusters)/list-clusters/$clusterName",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...eksClusterQueryOptions(params.clusterName),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...eksNodegroupsQueryOptions(params.clusterName),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...eksAccessEntriesQueryOptions(params.clusterName),
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.clusterName} | EKS | Mulga` }],
  }),
  component: ClusterDetail,
})

function ClusterDetail() {
  const { clusterName } = Route.useParams()
  return <ClusterDetailPage clusterName={clusterName} />
}
