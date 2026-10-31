import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ec2AddressesQueryOptions } from "@/queries/ec2"

import { ElasticIpsPage } from "../-components/elastic-ips-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(elastic-ips)/describe-addresses/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2AddressesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Elastic IPs | EC2 | Mulga",
      },
    ],
  }),
  component: Addresses,
})

function Addresses() {
  const { data } = useSuspenseQuery(ec2AddressesQueryOptions)
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
    <ElasticIpsPage
      addresses={data.Addresses ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
