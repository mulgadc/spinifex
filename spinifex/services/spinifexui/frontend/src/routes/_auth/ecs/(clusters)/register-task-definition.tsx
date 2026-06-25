import { createFileRoute, type SearchSchemaInput } from "@tanstack/react-router"

import { RegisterTaskDefinitionPage } from "./-components/register-task-definition-page"

interface RegisterSearch {
  cluster: string
}

export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/register-task-definition",
)({
  validateSearch: (search: RegisterSearch & SearchSchemaInput) => ({
    cluster: typeof search.cluster === "string" ? search.cluster : "",
  }),
  head: () => ({
    meta: [{ title: "Register Task Definition | ECS | Mulga" }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { cluster } = Route.useSearch()
  return <RegisterTaskDefinitionPage cluster={cluster} />
}
