import { createFileRoute, type SearchSchemaInput } from "@tanstack/react-router"

import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import { ecsTaskDefinitionsQueryOptions } from "@/queries/ecs"
import { elbv2TargetGroupsQueryOptions } from "@/queries/elbv2"

import { CreateServicePage } from "./-components/create-service-page"

interface CreateServiceSearch {
  cluster: string
}

export const Route = createFileRoute("/_auth/ecs/(clusters)/create-service")({
  validateSearch: (search: CreateServiceSearch & SearchSchemaInput) => ({
    cluster: typeof search.cluster === "string" ? search.cluster : "",
  }),
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
