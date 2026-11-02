import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { rdsSubnetGroupsQueryOptions } from "@/queries/rds"

import { DBSubnetGroupsListPage } from "../-components/db-subnet-groups-list-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/rds/(subnet-groups)/describe-db-subnet-groups/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...rdsSubnetGroupsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Subnet Groups | RDS | Mulga",
      },
    ],
  }),
  component: DBSubnetGroups,
})

function DBSubnetGroups() {
  const { data } = useSuspenseQuery(rdsSubnetGroupsQueryOptions)
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
    <DBSubnetGroupsListPage
      groups={data.DBSubnetGroups ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
