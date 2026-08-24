import { createFileRoute } from "@tanstack/react-router"
import { z } from "zod"

import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import { ecsTaskDefinitionsQueryOptions } from "@/queries/ecs"

import { RunTaskPage } from "./-components/run-task-page"

/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({ cluster: z.string().catch("") })
/* oxlint-enable promise/prefer-await-to-then */

export const Route = createFileRoute("/_auth/ecs/(clusters)/run-task")({
  validateSearch: searchSchema,
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ecsTaskDefinitionsQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
    ])
  },
  head: () => ({
    meta: [{ title: "Run Task | ECS | Mulga" }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { cluster } = Route.useSearch()
  return <RunTaskPage cluster={cluster} />
}
