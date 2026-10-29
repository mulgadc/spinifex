import type { Task } from "@aws-sdk/client-ecs"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Play } from "lucide-react"
import { useState } from "react"

import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { arnResourceId } from "@/lib/utils"
import { useStopTask } from "@/mutations/ecs"
import { ecsTasksQueryOptions } from "@/queries/ecs"

import { PanelList, PanelListItem } from "./panel-list"
import { StopTaskDialog } from "./stop-task-dialog"

export function ClusterTasks({ clusterName }: { clusterName: string }) {
  const { data: tasks } = useSuspenseQuery(ecsTasksQueryOptions(clusterName))
  const stopTask = useStopTask()
  const [stopTarget, setStopTarget] = useState<string | null>(null)

  return (
    <PanelSection title={`Tasks (${tasks.length})`}>
      <div className="flex flex-col items-start gap-3">
        {stopTask.isError && (
          <ErrorBanner error={stopTask.error} msg="Failed to stop task." />
        )}
        {tasks.length === 0 ? (
          <p className="text-sm text-muted-foreground">No tasks found.</p>
        ) : (
          <div className="w-full">
            <PanelList>
              {tasks.map((task: Task) => {
                const id = arnResourceId(task.taskArn)

                return (
                  <PanelListItem
                    actions={
                      <Button
                        disabled={task.lastStatus === "STOPPED"}
                        onClick={() => {
                          setStopTarget(task.taskArn ?? id)
                        }}
                        size="sm"
                        variant="destructive"
                      >
                        Stop
                      </Button>
                    }
                    detail={[`Desired ${task.desiredStatus ?? "—"}`, task.group]
                      .filter(Boolean)
                      .join(" · ")}
                    key={id}
                    state={<StateBadge state={task.lastStatus} />}
                    title={
                      <Link
                        className="font-mono text-xs text-primary hover:underline"
                        params={{ clusterName, taskId: id }}
                        to="/ecs/list-clusters/$clusterName/tasks/$taskId"
                      >
                        {id}
                      </Link>
                    }
                  />
                )
              })}
            </PanelList>
          </div>
        )}
        <Link
          className={buttonVariants({ size: "sm", variant: "outline" })}
          search={{ cluster: clusterName }}
          to="/ecs/run-task"
        >
          <Play data-icon="inline-start" />
          Run task
        </Link>
      </div>
      <StopTaskDialog
        clusterName={clusterName}
        mutation={stopTask}
        onOpenChange={(open) => {
          if (!open) {
            setStopTarget(null)
          }
        }}
        task={stopTarget}
      />
    </PanelSection>
  )
}
