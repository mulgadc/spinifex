import type { Reservation } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { panelSearchFields } from "@/lib/panel-search"
import { ec2InstancesQueryOptions } from "@/queries/ec2"

import {
  CLOSED_INSTANCES_PANEL,
  type InstancesSearch,
  InstancesPage,
  STATE_FILTERS,
} from "../-components/instances-page"

// Defaults keep every param optional to links; catch drops a mangled value.
/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({
  q: z.string().default("").catch(""),
  state: z.enum(STATE_FILTERS).default("all").catch("all"),
  sort: z.string().default("").catch(""),
  template: z.string().default("").catch(""),
  templateVersion: z.string().default("").catch(""),
  ...panelSearchFields,
})
/* oxlint-enable promise/prefer-await-to-then */

const DEFAULT_SEARCH = {
  q: "",
  state: "all",
  sort: "",
  ...CLOSED_INSTANCES_PANEL,
} as const

export const Route = createFileRoute(
  "/_auth/ec2/(instances)/describe-instances/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(DEFAULT_SEARCH)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2InstancesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Instances | EC2 | Mulga",
      },
    ],
  }),
  component: Ec2,
})

// oxlint-disable-next-line anti-slop/no-known-value-widening -- keyed by the state name the SDK reports, not by a closed set
const statePriority: Record<string, number> = {
  running: 0,
  pending: 1,
  stopping: 2,
  "shutting-down": 3,
  stopped: 4,
  terminated: 5,
}

function Ec2() {
  const { data } = useSuspenseQuery(ec2InstancesQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Unsorted, the list keeps live instances first, as it always has.
  const instances = (
    data.Reservations?.flatMap(
      (reservation: Reservation) => reservation.Instances ?? [],
    ) ?? []
  ).toSorted((a, b) => {
    const pa = statePriority[a.State?.Name ?? ""] ?? 6
    const pb = statePriority[b.State?.Name ?? ""] ?? 6

    if (pa !== pb) {
      return pa - pb
    }

    return (a.InstanceId ?? "").localeCompare(b.InstanceId ?? "")
  })

  // Filters replace the history entry; opening or closing a panel adds one, so
  // Back closes the panel it opened.
  function onSearchChange(next: Partial<InstancesSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return (
    <InstancesPage
      instances={instances}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
