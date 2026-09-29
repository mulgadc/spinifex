import { Link } from "@tanstack/react-router"
import { useState } from "react"

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useDeleteDBInstance } from "@/mutations/rds"
import { dbSnapshotIdentifierField, suggestedIdentifier } from "@/types/rds"

export function defaultFinalSnapshotIdentifier(
  dbInstanceIdentifier: string,
  now: Date = new Date(),
): string {
  return suggestedIdentifier(dbInstanceIdentifier, "final", now)
}

interface DeleteDBInstanceDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  dbInstanceIdentifier: string
  deletionProtection: boolean
  onDeleted?: () => void
  // Supplied by the detail page, which owns the modify dialog. Without it the
  // deletion-protection notice links to the instance instead.
  onModify?: () => void
}

export function DeleteDBInstanceDialog({
  open,
  onOpenChange,
  dbInstanceIdentifier,
  deletionProtection,
  onDeleted,
  onModify,
}: DeleteDBInstanceDialogProps) {
  const deleteInstance = useDeleteDBInstance()
  const [skipFinalSnapshot, setSkipFinalSnapshot] = useState(false)
  const [snapshotIdentifier, setSnapshotIdentifier] = useState(() =>
    defaultFinalSnapshotIdentifier(dbInstanceIdentifier),
  )
  const [typedIdentifier, setTypedIdentifier] = useState("")

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      setSkipFinalSnapshot(false)
      setSnapshotIdentifier(
        defaultFinalSnapshotIdentifier(dbInstanceIdentifier),
      )
      setTypedIdentifier("")
      deleteInstance.reset()
    }
    onOpenChange(nextOpen)
  }

  const snapshotNameError = skipFinalSnapshot
    ? undefined
    : dbSnapshotIdentifierField.safeParse(snapshotIdentifier).error?.issues[0]
        ?.message

  const confirmationMissing =
    skipFinalSnapshot && typedIdentifier !== dbInstanceIdentifier
  const canDelete = !confirmationMissing && !snapshotNameError

  async function handleDelete() {
    try {
      await deleteInstance.mutateAsync({
        dbInstanceIdentifier,
        skipFinalSnapshot,
        finalSnapshotIdentifier: snapshotIdentifier,
      })
      handleOpenChange(false)
      onDeleted?.()
    } catch {
      // Surfaced below via deleteInstance.error
    }
  }

  return (
    <AlertDialog onOpenChange={handleOpenChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Delete DB Instance</AlertDialogTitle>
          <AlertDialogDescription>
            {deletionProtection
              ? `"${dbInstanceIdentifier}" has deletion protection enabled and cannot be deleted while it is on.`
              : `This tears down "${dbInstanceIdentifier}". It cannot be undone.`}
          </AlertDialogDescription>
        </AlertDialogHeader>

        {deletionProtection ? (
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">
              Turn deletion protection off first, then delete the instance.
            </p>
            {onModify ? (
              <Button onClick={onModify} size="sm" variant="outline">
                Modify Instance
              </Button>
            ) : (
              <Link
                className="text-sm text-primary hover:underline"
                params={{ id: dbInstanceIdentifier }}
                to="/rds/describe-db-instances/$id"
              >
                Open {dbInstanceIdentifier} to modify it
              </Link>
            )}
          </div>
        ) : (
          <div className="space-y-4">
            <label className="flex items-start gap-2 text-sm">
              <input
                checked={!skipFinalSnapshot}
                onChange={(e) => {
                  setSkipFinalSnapshot(!e.target.checked)
                }}
                type="checkbox"
              />
              <span>
                Take a final snapshot before deleting
                <span className="mt-1 block text-xs text-muted-foreground">
                  A final snapshot retains the underlying data volume until that
                  snapshot is itself deleted.
                </span>
              </span>
            </label>

            {skipFinalSnapshot ? (
              <Field>
                <FieldTitle>
                  <label htmlFor="confirmIdentifier">
                    Type {dbInstanceIdentifier} to confirm
                  </label>
                </FieldTitle>
                <Input
                  id="confirmIdentifier"
                  onChange={(e) => {
                    setTypedIdentifier(e.target.value)
                  }}
                  placeholder={dbInstanceIdentifier}
                  value={typedIdentifier}
                />
                <FieldDescription>
                  Deleting without a final snapshot destroys the data volume.
                  There is no way back.
                </FieldDescription>
              </Field>
            ) : (
              <Field>
                <FieldTitle>
                  <label htmlFor="finalSnapshotIdentifier">
                    Final snapshot name
                  </label>
                </FieldTitle>
                <Input
                  aria-invalid={!!snapshotNameError}
                  id="finalSnapshotIdentifier"
                  onChange={(e) => {
                    setSnapshotIdentifier(e.target.value)
                  }}
                  value={snapshotIdentifier}
                />
                {snapshotNameError && (
                  <p className="text-xs text-destructive">
                    {snapshotNameError}
                  </p>
                )}
              </Field>
            )}
          </div>
        )}

        {deleteInstance.error && (
          <p className="text-sm text-destructive">
            {deleteInstance.error.message}
          </p>
        )}

        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          {!deletionProtection && (
            <AlertDialogAction
              disabled={deleteInstance.isPending || !canDelete}
              onClick={(e) => {
                e.preventDefault()
                void handleDelete()
              }}
            >
              {deleteInstance.isPending ? "Deleting…" : "Delete"}
            </AlertDialogAction>
          )}
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
