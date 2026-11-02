import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { panelSearchFields } from "@/lib/panel-search"
import {
  eksAccessEntriesQueryOptions,
  eksClusterQueryOptions,
  eksNodegroupsQueryOptions,
} from "@/queries/eks"

import { ClusterDetails } from "../-components/cluster-details"

export const Route = createFileRoute(
  "/_auth/eks/(clusters)/list-clusters/$clusterName",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
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

// The cluster's own page: the side panel's tabs, laid out for the full width.
function ClusterDetail() {
  const { clusterName } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()

  return (
    <ClusterDetails
      clusterName={clusterName}
      layout="page"
      onClose={() => {
        void navigate({ to: "/eks/list-clusters" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
