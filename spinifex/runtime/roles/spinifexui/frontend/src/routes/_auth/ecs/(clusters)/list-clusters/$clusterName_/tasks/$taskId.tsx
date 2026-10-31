import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ecsTasksQueryOptions } from "@/queries/ecs"

import { TaskDetails } from "../../../-components/task-details"

export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/list-clusters/$clusterName_/tasks/$taskId",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ecsTasksQueryOptions(params.clusterName),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.taskId} | ECS | Mulga` }],
  }),
  component: TaskDetail,
})

// The task's own page, inside its cluster.
function TaskDetail() {
  const { clusterName, taskId } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data: tasks } = useSuspenseQuery(ecsTasksQueryOptions(clusterName))
  const task = tasks.find((t) => (t.taskArn ?? "").endsWith(taskId))

  if (!task) {
    return (
      <>
        <BackLink params={{ clusterName }} to="/ecs/list-clusters/$clusterName">
          {clusterName}
        </BackLink>
        <p className="text-muted-foreground">Task not found.</p>
      </>
    )
  }

  return (
    <TaskDetails
      clusterName={clusterName}
      layout="page"
      onClose={() => {
        void navigate({
          to: "/ecs/list-clusters/$clusterName",
          params: { clusterName },
        })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
      task={task}
    />
  )
}
