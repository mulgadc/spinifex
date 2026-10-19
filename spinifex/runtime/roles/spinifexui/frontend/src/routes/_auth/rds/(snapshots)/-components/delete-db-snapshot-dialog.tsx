import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
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
  return (
    <ResourceDeleteDialog
      description={
        <>
          This deletes the DB snapshot &quot;{dbSnapshotIdentifier}&quot; and
          the data behind it. It is refused while an instance restored from it
          still exists. If this was a final snapshot, deleting it also releases
          the data volume its instance left behind.
        </>
      }
      identifier={dbSnapshotIdentifier}
      mutation={useDeleteDBSnapshot()}
      onDeleted={onDeleted}
      onOpenChange={onOpenChange}
      open={open}
      title="Delete DB Snapshot"
    />
  )
}
