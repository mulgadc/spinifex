import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { rdsDBInstancesQueryOptions } from "@/queries/rds"

import { DBInstancesListPage } from "../-components/db-instances-list-page"

export const Route = createFileRoute(
  "/_auth/rds/(db-instances)/describe-db-instances/",
)({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...rdsDBInstancesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Databases | RDS | Mulga",
      },
    ],
  }),
  component: DBInstances,
})

function DBInstances() {
  const { data } = useSuspenseQuery(rdsDBInstancesQueryOptions)
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
    <DBInstancesListPage
      instances={data.DBInstances ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
