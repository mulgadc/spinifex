import { createFileRoute, redirect } from "@tanstack/react-router"
import { z } from "zod"

/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({
  launchTemplateId: z.string().default("").catch(""),
  launchTemplateVersion: z.string().default("").catch(""),
})
/* oxlint-enable promise/prefer-await-to-then */

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute("/_auth/ec2/(instances)/run-instances")({
  validateSearch: searchSchema,
  beforeLoad: ({ search }) => {
    throw redirect({
      to: "/ec2/describe-instances",
      search: {
        create: true,
        template: search.launchTemplateId,
        templateVersion: search.launchTemplateVersion,
      },
    })
  },
})
