import { createFileRoute } from "@tanstack/react-router"
import { z } from "zod"

import { RegisterTaskDefinitionPage } from "./-components/register-task-definition-page"

/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({ cluster: z.string().catch("") })
/* oxlint-enable promise/prefer-await-to-then */

export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/register-task-definition",
)({
  validateSearch: searchSchema,
  head: () => ({
    meta: [{ title: "Register Task Definition | ECS | Mulga" }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { cluster } = Route.useSearch()
  return <RegisterTaskDefinitionPage cluster={cluster} />
}
