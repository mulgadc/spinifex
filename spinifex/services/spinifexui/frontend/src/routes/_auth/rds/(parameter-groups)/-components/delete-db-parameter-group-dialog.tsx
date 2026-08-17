import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { useDeleteDBParameterGroup } from "@/mutations/rds"

interface DeleteDBParameterGroupDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  dbParameterGroupName: string
  onDeleted?: () => void
}

export function DeleteDBParameterGroupDialog({
  open,
  onOpenChange,
  dbParameterGroupName,
  onDeleted,
}: DeleteDBParameterGroupDialogProps) {
  const deleteGroup = useDeleteDBParameterGroup()

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      deleteGroup.reset()
    }
    onOpenChange(nextOpen)
  }

  async function handleDelete() {
    try {
      await deleteGroup.mutateAsync(dbParameterGroupName)
      handleOpenChange(false)
      onDeleted?.()
    } catch {
      // Left open so the refusal below stays readable.
    }
  }

  // The refusal names every instance still holding the group, which is the
  // useful half of the failure, so it is rendered rather than summarised.
  const description = (
    <>
      This deletes the DB parameter group &quot;{dbParameterGroupName}&quot; and
      every value stored on it. It is refused while any DB instance references
      the group, including one whose pending changes name it.
      {deleteGroup.error && (
        <span className="mt-2 block text-destructive">
          {deleteGroup.error.message}
        </span>
      )}
    </>
  )

  return (
    <DeleteConfirmationDialog
      description={description}
      isPending={deleteGroup.isPending}
      onConfirm={() => void handleDelete()}
      onOpenChange={handleOpenChange}
      open={open}
      title="Delete DB parameter group"
    />
  )
}
