import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { isEcsSystemImage } from "@/lib/system-managed"
import { ec2ImagesQueryOptions } from "@/queries/ec2"
import { ecsClustersQueryOptions } from "@/queries/ecs"

import { ClustersPage } from "../-components/clusters-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute("/_auth/ecs/(clusters)/list-clusters/")({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
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
  component: Clusters,
})

function Clusters() {
  const { data: clusters } = useSuspenseQuery(ecsClustersQueryOptions)
  const { data: images } = useSuspenseQuery(ec2ImagesQueryOptions)
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
    <ClustersPage
      clusters={clusters}
      hasSystemImage={(images.Images ?? []).some(isEcsSystemImage)}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
