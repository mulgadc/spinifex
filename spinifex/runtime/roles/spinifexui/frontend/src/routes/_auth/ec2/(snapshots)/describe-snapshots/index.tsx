import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ec2SnapshotsQueryOptions } from "@/queries/ec2"

import { SnapshotsPage } from "../-components/snapshots-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(snapshots)/describe-snapshots/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2SnapshotsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Snapshots | EC2 | Mulga",
      },
    ],
  }),
  component: Snapshots,
})

function Snapshots() {
  const { data } = useSuspenseQuery(ec2SnapshotsQueryOptions)
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
    <SnapshotsPage
      onSearchChange={onSearchChange}
      search={search}
      snapshots={data.Snapshots ?? []}
    />
  )
}
