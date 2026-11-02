import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { elbv2LoadBalancersQueryOptions } from "@/queries/elbv2"

import { DescribeLoadBalancersPage } from "../-components/describe-load-balancers-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(load-balancers)/describe-load-balancers/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...elbv2LoadBalancersQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Load Balancers | EC2 | Mulga",
      },
    ],
  }),
  component: LoadBalancers,
})

function LoadBalancers() {
  const { data } = useSuspenseQuery(elbv2LoadBalancersQueryOptions)
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
    <DescribeLoadBalancersPage
      loadBalancers={data.LoadBalancers ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
