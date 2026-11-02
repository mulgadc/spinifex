import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { eksClustersQueryOptions } from "@/queries/eks"

import { ClustersListPage } from "../-components/clusters-list-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute("/_auth/eks/(clusters)/list-clusters/")({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...eksClustersQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Clusters | EKS | Mulga" }],
  }),
  component: Clusters,
})

function Clusters() {
  const { data } = useSuspenseQuery(eksClustersQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return (
    <ClustersListPage
      clusters={data.clusters ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
