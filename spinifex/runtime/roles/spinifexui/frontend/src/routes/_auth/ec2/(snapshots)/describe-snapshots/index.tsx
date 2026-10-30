import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { panelSearchFields } from "@/lib/panel-search"
import { ec2SnapshotsQueryOptions } from "@/queries/ec2"

import {
  CLOSED_SNAPSHOTS_PANEL,
  type SnapshotsSearch,
  SnapshotsPage,
} from "../-components/snapshots-page"

/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({
  ...panelSearchFields,
  volume: z.string().default("").catch(""),
})
/* oxlint-enable promise/prefer-await-to-then */

export const Route = createFileRoute(
  "/_auth/ec2/(snapshots)/describe-snapshots/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_SNAPSHOTS_PANEL)] },
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
  function onSearchChange(next: Partial<SnapshotsSearch>) {
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
