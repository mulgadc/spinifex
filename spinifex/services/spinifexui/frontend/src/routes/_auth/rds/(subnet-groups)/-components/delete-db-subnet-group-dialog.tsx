import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { useDeleteDBSubnetGroup } from "@/mutations/rds"

interface DeleteDBSubnetGroupDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  dbSubnetGroupName: string
  onDeleted?: () => void
}

export function DeleteDBSubnetGroupDialog({
  open,
  onOpenChange,
  dbSubnetGroupName,
  onDeleted,
}: DeleteDBSubnetGroupDialogProps) {
  const deleteGroup = useDeleteDBSubnetGroup()

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      deleteGroup.reset()
    }
    onOpenChange(nextOpen)
  }

  async function handleDelete() {
    try {
      await deleteGroup.mutateAsync(dbSubnetGroupName)
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
      This deletes the DB subnet group &quot;{dbSubnetGroupName}&quot;. It is
      refused while any DB instance still references it, including one that is
      only deleting.
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
      title="Delete DB subnet group"
    />
  )
}
