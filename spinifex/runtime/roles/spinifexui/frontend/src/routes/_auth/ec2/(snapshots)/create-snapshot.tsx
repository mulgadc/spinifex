import { createFileRoute, redirect } from "@tanstack/react-router"
import { z } from "zod"

/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({
  volumeId: z.string().default("").catch(""),
})
/* oxlint-enable promise/prefer-await-to-then */

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute("/_auth/ec2/(snapshots)/create-snapshot")({
  validateSearch: searchSchema,
  beforeLoad: ({ search }) => {
    throw redirect({
      to: "/ec2/describe-snapshots",
      search: { create: true, volume: search.volumeId },
    })
  },
})
