import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"

import { ecsClustersQueryOptions } from "@/queries/ecs"

import { ClustersPage } from "../-components/clusters-page"
import {
  CLOSED_CLUSTERS_PANEL,
  type ClustersSearch,
  clustersSearchSchema,
} from "../-components/clusters-search"

export const Route = createFileRoute("/_auth/ecs/(clusters)/list-clusters/")({
  validateSearch: clustersSearchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_CLUSTERS_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ecsClustersQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Clusters | ECS | Mulga" }],
  }),
  component: Clusters,
})

function Clusters() {
  const { data: clusters } = useSuspenseQuery(ecsClustersQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<ClustersSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next || "launch" in next),
    })
  }

  return (
    <ClustersPage
      clusters={clusters}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
