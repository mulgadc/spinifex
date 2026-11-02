import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ec2SecurityGroupsQueryOptions } from "@/queries/ec2"

import { SecurityGroupsPage } from "../-components/security-groups-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(security-groups)/describe-security-groups/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2SecurityGroupsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Security Groups | EC2 | Mulga",
      },
    ],
  }),
  component: SecurityGroups,
})

function SecurityGroups() {
  const { data } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
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
    <SecurityGroupsPage
      onSearchChange={onSearchChange}
      search={search}
      securityGroups={data.SecurityGroups ?? []}
    />
  )
}
