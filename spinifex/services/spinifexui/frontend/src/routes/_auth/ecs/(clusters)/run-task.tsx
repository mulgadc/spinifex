import { createFileRoute, type SearchSchemaInput } from "@tanstack/react-router"

import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import { ecsTaskDefinitionsQueryOptions } from "@/queries/ecs"

import { RunTaskPage } from "./-components/run-task-page"

interface RunTaskSearch {
  cluster: string
}

export const Route = createFileRoute("/_auth/ecs/(clusters)/run-task")({
  validateSearch: (search: RunTaskSearch & SearchSchemaInput) => ({
    cluster: typeof search.cluster === "string" ? search.cluster : "",
  }),
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
