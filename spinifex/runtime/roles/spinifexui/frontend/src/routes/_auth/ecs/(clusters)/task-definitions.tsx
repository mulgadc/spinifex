import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ecsTaskDefinitionsQueryOptions } from "@/queries/ecs"

import { TaskDefinitionsPage } from "./-components/task-definitions-page"

export const Route = createFileRoute("/_auth/ecs/(clusters)/task-definitions")({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ecsTaskDefinitionsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Task Definitions | ECS | Mulga" }],
  }),
  component: TaskDefinitions,
})

function TaskDefinitions() {
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return <TaskDefinitionsPage onSearchChange={onSearchChange} search={search} />
}
