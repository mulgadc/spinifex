import { createFileRoute, redirect } from "@tanstack/react-router"
import { z } from "zod"

/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({ cluster: z.string().catch("") })
/* oxlint-enable promise/prefer-await-to-then */

// The form opens in the clusters list's side panel; this address is kept for
// links.
export const Route = createFileRoute("/_auth/ecs/(clusters)/create-service")({
  validateSearch: searchSchema,
  beforeLoad: ({ search }) => {
    throw redirect({
      to: "/ecs/list-clusters",
      search: search.cluster
        ? { selected: search.cluster, launch: "service" }
        : {},
    })
  },
})
