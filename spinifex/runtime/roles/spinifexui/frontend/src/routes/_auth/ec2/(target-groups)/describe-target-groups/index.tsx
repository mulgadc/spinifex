import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { elbv2TargetGroupsQueryOptions } from "@/queries/elbv2"

import { DescribeTargetGroupsPage } from "../-components/describe-target-groups-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(target-groups)/describe-target-groups/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...elbv2TargetGroupsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Target Groups | EC2 | Mulga",
      },
    ],
  }),
  component: TargetGroups,
})

function TargetGroups() {
  const { data } = useSuspenseQuery(elbv2TargetGroupsQueryOptions)
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
    <DescribeTargetGroupsPage
      targetGroups={data.TargetGroups ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
