import { useDeleteDBSnapshot } from "@/mutations/rds"

import { RdsDeleteDialog } from "../../-components/rds-delete-dialog"

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
    <RdsDeleteDialog
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
