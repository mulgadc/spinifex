import { createFileRoute } from "@tanstack/react-router"

import { ecsTaskDefinitionsQueryOptions } from "@/queries/ecs"

import { TaskDefinitionsPage } from "./-components/task-definitions-page"

export const Route = createFileRoute("/_auth/ecs/(clusters)/task-definitions")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ecsTaskDefinitionsQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Task Definitions | ECS | Mulga" }],
  }),
  component: TaskDefinitionsPage,
})
