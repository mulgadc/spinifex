import { createFileRoute } from "@tanstack/react-router"

import { ecsTasksQueryOptions } from "@/queries/ecs"

import { TaskDetailPage } from "../../../-components/task-detail-page"

export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/list-clusters/$clusterName_/tasks/$taskId",
)({
  loader: async ({ context, params }) => {
    await context.queryClient.ensureQueryData(
      ecsTasksQueryOptions(params.clusterName),
    )
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.taskId} | ECS | Mulga` }],
  }),
  component: TaskDetail,
})

function TaskDetail() {
  const { clusterName, taskId } = Route.useParams()
  return <TaskDetailPage clusterName={clusterName} taskId={taskId} />
}
