import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { rdsDBSnapshotsQueryOptions } from "@/queries/rds"

import { DBSnapshotsListPage } from "../-components/db-snapshots-list-page"

export const Route = createFileRoute(
  "/_auth/rds/(snapshots)/describe-db-snapshots/",
)({
  // instance fixes the create form's source, as a database's Take snapshot does.
  validateSearch: z.object({
    ...panelSearchFields,
    // oxlint-disable-next-line promise/prefer-await-to-then -- zod's schema fallback
    instance: z.string().default("").catch(""),
  }),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: {
    middlewares: [stripSearchParams({ ...CLOSED_PANEL, instance: "" })],
  },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...rdsDBSnapshotsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Snapshots | RDS | Mulga",
      },
    ],
  }),
  component: DBSnapshots,
})

function DBSnapshots() {
  const { data } = useSuspenseQuery(rdsDBSnapshotsQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, instance: "", ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return (
    <DBSnapshotsListPage
      instance={search.instance}
      onSearchChange={onSearchChange}
      search={search}
      snapshots={data.DBSnapshots ?? []}
    />
  )
}
