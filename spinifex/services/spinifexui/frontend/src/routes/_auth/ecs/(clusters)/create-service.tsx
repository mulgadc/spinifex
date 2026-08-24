import { createFileRoute } from "@tanstack/react-router"
import { z } from "zod"

import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import { ecsTaskDefinitionsQueryOptions } from "@/queries/ecs"
import { elbv2TargetGroupsQueryOptions } from "@/queries/elbv2"

import { CreateServicePage } from "./-components/create-service-page"

/* oxlint-disable promise/prefer-await-to-then -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({ cluster: z.string().catch("") })
/* oxlint-enable promise/prefer-await-to-then */

export const Route = createFileRoute("/_auth/ecs/(clusters)/create-service")({
  validateSearch: searchSchema,
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ecsTaskDefinitionsQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
      context.queryClient.ensureQueryData(elbv2TargetGroupsQueryOptions),
    ])
  },
  head: () => ({
    meta: [{ title: "Create Service | ECS | Mulga" }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { cluster } = Route.useSearch()
  return <CreateServicePage cluster={cluster} />
}
