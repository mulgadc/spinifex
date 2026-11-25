import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ec2TransitGatewaysQueryOptions } from "@/queries/ec2"

import { TransitGatewaysPage } from "../-components/transit-gateways-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(transit-gateways)/describe-transit-gateways/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2TransitGatewaysQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Transit Gateways | EC2",
      },
    ],
  }),
  component: TransitGateways,
})

function TransitGateways() {
  const { data } = useSuspenseQuery(ec2TransitGatewaysQueryOptions)
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
    <TransitGatewaysPage
      onSearchChange={onSearchChange}
      search={search}
      transitGateways={data.TransitGateways ?? []}
    />
  )
}
