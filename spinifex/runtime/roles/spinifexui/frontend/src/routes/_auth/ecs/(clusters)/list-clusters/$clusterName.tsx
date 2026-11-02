import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  ecsClusterQueryOptions,
  ecsServicesQueryOptions,
  ecsTasksQueryOptions,
} from "@/queries/ecs"

import { ClusterDetails } from "../-components/cluster-details"

export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/list-clusters/$clusterName",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
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

// The cluster's own page: the side panel's tabs, laid out for the full width.
function ClusterDetail() {
  const { clusterName } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()

  const { data: cluster } = useSuspenseQuery(
    ecsClusterQueryOptions(clusterName),
  )

  if (!cluster) {
    return (
      <>
        <BackLink to="/ecs/list-clusters">Back to clusters</BackLink>
        <p className="text-muted-foreground">Cluster not found.</p>
      </>
    )
  }

  return (
    <ClusterDetails
      cluster={cluster}
      layout="page"
      onClose={() => {
        void navigate({ to: "/ecs/list-clusters" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
