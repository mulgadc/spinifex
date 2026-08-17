import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { useDeleteDBSnapshot } from "@/mutations/rds"

interface DeleteDBSnapshotDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  dbSnapshotIdentifier: string
  onDeleted?: () => void
}

export function DeleteDBSnapshotDialog({
  open,
  onOpenChange,
  dbSnapshotIdentifier,
  onDeleted,
}: DeleteDBSnapshotDialogProps) {
  const deleteSnapshot = useDeleteDBSnapshot()

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      deleteSnapshot.reset()
    }
    onOpenChange(nextOpen)
  }

  async function handleDelete() {
    try {
      await deleteSnapshot.mutateAsync(dbSnapshotIdentifier)
      handleOpenChange(false)
      onDeleted?.()
    } catch {
      // Left open so the refusal below stays readable.
    }
  }

  // The refusal names the restored instance still reading through the snapshot,
  // which is the useful half of that failure, so it is rendered in full.
  const description = (
    <>
      This deletes the DB snapshot &quot;{dbSnapshotIdentifier}&quot; and the
      data behind it. It is refused while an instance restored from it still
      exists. If this was a final snapshot, deleting it also releases the data
      volume its instance left behind.
      {deleteSnapshot.error && (
        <span className="mt-2 block text-destructive">
          {deleteSnapshot.error.message}
        </span>
      )}
    </>
  )

  return (
    <DeleteConfirmationDialog
      description={description}
      isPending={deleteSnapshot.isPending}
      onConfirm={() => void handleDelete()}
      onOpenChange={handleOpenChange}
      open={open}
      title="Delete DB snapshot"
    />
  )
}
