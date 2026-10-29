import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { route53HostedZonesQueryOptions } from "@/queries/route53"

import { ListHostedZonesPage } from "../-components/list-hosted-zones-page"

export const Route = createFileRoute(
  "/_auth/route53/(hosted-zones)/list-hosted-zones/",
)({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...route53HostedZonesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Hosted zones | DNS | Mulga",
      },
    ],
  }),
  component: HostedZones,
})

function HostedZones() {
  const { data: zones } = useSuspenseQuery(route53HostedZonesQueryOptions)
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
    <ListHostedZonesPage
      onSearchChange={onSearchChange}
      search={search}
      zones={zones}
    />
  )
}
