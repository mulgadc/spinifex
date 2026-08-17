import { useDeleteDBSubnetGroup } from "@/mutations/rds"

import { RdsDeleteDialog } from "../../-components/rds-delete-dialog"

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
  return (
    <RdsDeleteDialog
      description={
        <>
          This deletes the DB subnet group &quot;{dbSubnetGroupName}&quot;. It
          is refused while any DB instance still references it, including one
          that is only deleting.
        </>
      }
      identifier={dbSubnetGroupName}
      mutation={useDeleteDBSubnetGroup()}
      onDeleted={onDeleted}
      onOpenChange={onOpenChange}
      open={open}
      title="Delete DB subnet group"
    />
  )
}
