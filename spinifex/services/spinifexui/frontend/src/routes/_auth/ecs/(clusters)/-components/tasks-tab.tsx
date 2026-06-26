import type { Task } from "@aws-sdk/client-ecs"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { useStopTask } from "@/mutations/ecs"
import { ecsTasksQueryOptions } from "@/queries/ecs"

function taskId(arn: string | undefined): string {
  if (!arn) {
    return ""
  }
  const idx = arn.lastIndexOf("/")
  return idx === -1 ? arn : arn.slice(idx + 1)
}

export function TasksTab({ clusterName }: { clusterName: string }) {
  const { data: tasks } = useSuspenseQuery(ecsTasksQueryOptions(clusterName))
  const stopTask = useStopTask()
  const [stopTarget, setStopTarget] = useState<string | null>(null)

  function handleStop() {
    if (!stopTarget) {
      return
    }
    stopTask.mutate(
      { cluster: clusterName, task: stopTarget },
      { onSuccess: () => setStopTarget(null) },
    )
  }

  return (
    <>
      <div className="mb-4 flex justify-end">
        <Link search={{ cluster: clusterName }} to="/ecs/run-task">
          <Button size="sm">Run task</Button>
        </Link>
      </div>

      {stopTask.isError && (
        <ErrorBanner
          error={stopTask.error instanceof Error ? stopTask.error : undefined}
          msg="Failed to stop task."
        />
      )}

      {tasks.length === 0 ? (
        <p className="text-muted-foreground">No tasks found.</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Task</th>
                <th className="px-4 py-2 font-medium">Last Status</th>
                <th className="px-4 py-2 font-medium">Desired Status</th>
                <th className="px-4 py-2 font-medium">Group</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {tasks.map((task: Task) => {
                const id = taskId(task.taskArn)
                const stoppable = task.lastStatus !== "STOPPED"
                return (
                  <tr className="border-b last:border-0" key={id}>
                    <td className="px-4 py-2 font-mono text-xs">{id}</td>
                    <td className="px-4 py-2">
                      <StateBadge state={task.lastStatus} />
                    </td>
                    <td className="px-4 py-2">{task.desiredStatus}</td>
                    <td className="px-4 py-2">{task.group}</td>
                    <td className="px-4 py-2 text-right">
                      {stoppable && (
                        <Button
                          onClick={() => setStopTarget(task.taskArn ?? id)}
                          size="sm"
                          variant="destructive"
                        >
                          Stop
                        </Button>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <DeleteConfirmationDialog
        confirmLabel="Stop task"
        description="This stops the task and releases its ENI and reserved capacity."
        isPending={stopTask.isPending}
        onConfirm={handleStop}
        onOpenChange={(open) => !open && setStopTarget(null)}
        open={stopTarget !== null}
        pendingLabel="Stopping…"
        title="Stop task"
      />
    </>
  )
}
