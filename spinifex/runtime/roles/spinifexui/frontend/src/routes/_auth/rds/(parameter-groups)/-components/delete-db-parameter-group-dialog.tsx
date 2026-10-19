import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
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
  return (
    <ResourceDeleteDialog
      description={
        <>
          This deletes the DB parameter group &quot;{dbParameterGroupName}&quot;
          and every value stored on it. It is refused while any DB instance
          references the group, including one whose pending changes name it.
        </>
      }
      identifier={dbParameterGroupName}
      mutation={useDeleteDBParameterGroup()}
      onDeleted={onDeleted}
      onOpenChange={onOpenChange}
      open={open}
      title="Delete DB Parameter Group"
    />
  )
}
