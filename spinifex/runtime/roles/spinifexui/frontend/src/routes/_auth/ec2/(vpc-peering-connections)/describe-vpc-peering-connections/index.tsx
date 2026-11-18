import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ec2VpcPeeringConnectionsQueryOptions } from "@/queries/ec2"

import { VpcPeeringConnectionsPage } from "../-components/vpc-peering-connections-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(vpc-peering-connections)/describe-vpc-peering-connections/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2VpcPeeringConnectionsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Peering Connections | EC2 | Mulga",
      },
    ],
  }),
  component: VpcPeeringConnections,
})

function VpcPeeringConnections() {
  const { data } = useSuspenseQuery(ec2VpcPeeringConnectionsQueryOptions)
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
    <VpcPeeringConnectionsPage
      connections={data.VpcPeeringConnections ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
