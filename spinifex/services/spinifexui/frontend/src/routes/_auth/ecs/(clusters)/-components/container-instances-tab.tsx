import type { ContainerInstance } from "@aws-sdk/client-ecs"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import {
  useDeregisterContainerInstance,
  useUpdateContainerInstanceState,
} from "@/mutations/ecs"
import { ecsContainerInstancesQueryOptions } from "@/queries/ecs"

type Action = "drain" | "activate" | "deregister"

interface PendingAction {
  arn: string
  action: Action
}

const ACTION_LABELS: Record<Action, string> = {
  drain: "Drain",
  activate: "Activate",
  deregister: "Deregister",
}

const ACTION_DESCRIPTIONS: Record<Action, string> = {
  drain:
    "Draining force-stops the service-owned tasks on this instance so it can be retired.",
  activate:
    "This returns the instance to ACTIVE so the scheduler can place tasks on it again.",
  deregister:
    "This force-removes the container instance record. Any tasks it still holds are stopped.",
}

export function ContainerInstancesTab({
  clusterName,
}: {
  clusterName: string
}) {
  const { data: instances } = useSuspenseQuery(
    ecsContainerInstancesQueryOptions(clusterName),
  )
  const updateState = useUpdateContainerInstanceState()
  const deregister = useDeregisterContainerInstance()
  const [pending, setPending] = useState<PendingAction | null>(null)

  function handleConfirm() {
    if (!pending) {
      return
    }
    const onSuccess = () => setPending(null)
    if (pending.action === "deregister") {
      deregister.mutate(
        { cluster: clusterName, containerInstance: pending.arn },
        { onSuccess },
      )
      return
    }
    updateState.mutate(
      {
        cluster: clusterName,
        containerInstance: pending.arn,
        status: pending.action === "drain" ? "DRAINING" : "ACTIVE",
      },
      { onSuccess },
    )
  }

  const isError = updateState.isError || deregister.isError
  const error = updateState.error ?? deregister.error
  const isPending = updateState.isPending || deregister.isPending

  if (instances.length === 0) {
    return (
      <p className="text-muted-foreground">No container instances found.</p>
    )
  }

  return (
    <>
      {isError && (
        <ErrorBanner
          error={error instanceof Error ? error : undefined}
          msg="Container instance action failed."
        />
      )}

      <div className="overflow-x-auto rounded-lg border bg-card">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="px-4 py-2 font-medium">EC2 Instance</th>
              <th className="px-4 py-2 font-medium">Status</th>
              <th className="px-4 py-2 font-medium">Running</th>
              <th className="px-4 py-2 font-medium">Pending</th>
              <th className="px-4 py-2 font-medium">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {instances.map((instance: ContainerInstance) => {
              const arn = instance.containerInstanceArn ?? ""
              const draining = instance.status === "DRAINING"
              return (
                <tr className="border-b last:border-0" key={arn}>
                  <td className="px-4 py-2 font-mono text-xs">
                    {instance.ec2InstanceId}
                  </td>
                  <td className="px-4 py-2">
                    <StateBadge state={instance.status} />
                  </td>
                  <td className="px-4 py-2">
                    {instance.runningTasksCount ?? 0}
                  </td>
                  <td className="px-4 py-2">
                    {instance.pendingTasksCount ?? 0}
                  </td>
                  <td className="px-4 py-2 text-right">
                    <div className="flex justify-end gap-2">
                      <Button
                        aria-label={
                          draining
                            ? "Activate container instance"
                            : "Drain container instance"
                        }
                        onClick={() =>
                          setPending({
                            arn,
                            action: draining ? "activate" : "drain",
                          })
                        }
                        size="sm"
                        variant="outline"
                      >
                        {draining ? "Activate" : "Drain"}
                      </Button>
                      <Button
                        onClick={() =>
                          setPending({ arn, action: "deregister" })
                        }
                        size="sm"
                        variant="destructive"
                      >
                        Deregister
                      </Button>
                    </div>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      <DeleteConfirmationDialog
        confirmLabel={pending ? ACTION_LABELS[pending.action] : "Confirm"}
        description={pending ? ACTION_DESCRIPTIONS[pending.action] : ""}
        isPending={isPending}
        onConfirm={handleConfirm}
        onOpenChange={(open) => !open && setPending(null)}
        open={pending !== null}
        title="Container instance"
      />
    </>
  )
}
