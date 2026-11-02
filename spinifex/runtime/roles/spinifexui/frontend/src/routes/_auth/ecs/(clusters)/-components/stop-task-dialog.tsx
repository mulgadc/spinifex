import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import type { useStopTask } from "@/mutations/ecs"

interface StopTaskDialogProps {
  clusterName: string
  // The task to stop; null keeps the dialog closed.
  task: string | null
  mutation: ReturnType<typeof useStopTask>
  onOpenChange: (open: boolean) => void
  onStopped?: () => void
}

export function StopTaskDialog({
  clusterName,
  task,
  mutation,
  onOpenChange,
  onStopped,
}: StopTaskDialogProps) {
  return (
    <DeleteConfirmationDialog
      confirmLabel="Stop task"
      description="This stops the task and releases its ENI and reserved capacity."
      isPending={mutation.isPending}
      onConfirm={() => {
        if (!task) {
          return
        }

        mutation.mutate(
          { cluster: clusterName, task },
          {
            onSuccess: () => {
              onOpenChange(false)
              onStopped?.()
            },
          },
        )
      }}
      onOpenChange={onOpenChange}
      open={task !== null}
      pendingLabel="Stopping…"
      title="Stop task"
    />
  )
}
