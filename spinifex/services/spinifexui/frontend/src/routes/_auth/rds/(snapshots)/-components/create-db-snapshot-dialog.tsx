import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { useForm } from "react-hook-form"

import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useCreateDBSnapshot } from "@/mutations/rds"
import { rdsDBInstancesQueryOptions } from "@/queries/rds"
import {
  type CreateDBSnapshotFormData,
  canSnapshot,
  createDBSnapshotSchema,
  suggestedIdentifier,
} from "@/types/rds"

import { PickerNoticeText, pickerNotice } from "../../-components/picker-notice"
import { TagsFieldArray } from "../../-components/tags-field-array"

interface CreateDBSnapshotDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  // Fixed when the dialog is opened from an instance. Without it the dialog
  // picks the instance itself, which is how the snapshots list opens it.
  dbInstanceIdentifier?: string
  onCreated?: () => void
}

export function CreateDBSnapshotDialog({
  open,
  onOpenChange,
  dbInstanceIdentifier,
  onCreated,
}: CreateDBSnapshotDialogProps) {
  const createSnapshot = useCreateDBSnapshot()
  // Only the picker needs the list, so a dialog opened from an instance does
  // not fetch it at all.
  const instancesQuery = useQuery({
    ...rdsDBInstancesQueryOptions,
    enabled: dbInstanceIdentifier === undefined,
  })
  const [selectedInstance, setSelectedInstance] = useState(
    dbInstanceIdentifier ?? "",
  )

  // A snapshot is refused from anything but a settled instance, so an instance
  // mid-transition is not offered rather than offered and then refused.
  const instances = (instancesQuery.data?.DBInstances ?? []).filter(
    (instance) => canSnapshot(instance.DBInstanceStatus),
  )
  const instanceNotice = pickerNotice(instancesQuery, instances.length === 0, {
    loading: "Loading the DB instances…",
    failed: "Could not read the DB instances",
    empty:
      "No DB instance is available or stopped, which is what a snapshot is taken from.",
  })

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
  } = useForm<CreateDBSnapshotFormData>({
    resolver: zodResolver(createDBSnapshotSchema),
    defaultValues: {
      dbSnapshotIdentifier: dbInstanceIdentifier
        ? suggestedIdentifier(dbInstanceIdentifier, "snapshot")
        : "",
      tags: [],
    },
  })

  // Choosing the instance names the snapshot after it, which is the name the
  // user would otherwise type by hand.
  function handleInstanceChange(identifier: string | null) {
    setSelectedInstance(identifier ?? "")
    if (identifier) {
      setValue(
        "dbSnapshotIdentifier",
        suggestedIdentifier(identifier, "snapshot"),
        { shouldValidate: true },
      )
    }
  }

  const onSubmit = async (data: CreateDBSnapshotFormData) => {
    try {
      await createSnapshot.mutateAsync({
        ...data,
        dbInstanceIdentifier: selectedInstance,
      })
    } catch {
      // Left open so the refusal below stays readable.
      return
    }
    onOpenChange(false)
    onCreated?.()
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent className="max-h-[85vh] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>Take DB snapshot</AlertDialogTitle>
          <AlertDialogDescription>
            The engine is held at a checkpoint while the snapshot is taken, so
            the instance reads as backing-up until it finishes.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <form className="space-y-4" onSubmit={handleSubmit(onSubmit)}>
          {dbInstanceIdentifier ? (
            <Field>
              <FieldTitle>DB instance</FieldTitle>
              <p className="font-mono text-sm">{dbInstanceIdentifier}</p>
            </Field>
          ) : (
            <Field>
              <FieldTitle>
                <label htmlFor="snapshot-instance">DB instance</label>
              </FieldTitle>
              {instanceNotice ? (
                <PickerNoticeText notice={instanceNotice} />
              ) : (
                <Select
                  onValueChange={handleInstanceChange}
                  value={selectedInstance}
                >
                  <SelectTrigger className="w-full" id="snapshot-instance">
                    <SelectValue placeholder="Select a DB instance" />
                  </SelectTrigger>
                  <SelectContent>
                    {instances.map((instance) => (
                      <SelectItem
                        key={instance.DBInstanceIdentifier}
                        value={instance.DBInstanceIdentifier ?? ""}
                      >
                        {instance.DBInstanceIdentifier} (
                        {instance.DBInstanceStatus})
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            </Field>
          )}

          <Field>
            <FieldTitle>
              <label htmlFor="snapshot-identifier">Snapshot identifier</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.dbSnapshotIdentifier}
              id="snapshot-identifier"
              {...register("dbSnapshotIdentifier")}
            />
            <FieldDescription>
              Lowercase letters, digits and hyphens. The rds: namespace belongs
              to automated backups and cannot be used here.
            </FieldDescription>
            <FieldError errors={[errors.dbSnapshotIdentifier]} />
          </Field>

          <TagsFieldArray control={control} name="tags" />

          <p className="text-xs text-muted-foreground">
            A snapshot the engine could not be quiesced for is still taken and
            reported as crash consistent on the source instance&apos;s Backups
            tab. The snapshot&apos;s own events never carry it.
          </p>

          {createSnapshot.error && (
            <p className="text-sm text-destructive">
              {createSnapshot.error.message}
            </p>
          )}

          <div className="flex justify-end gap-2">
            <Button
              onClick={() => onOpenChange(false)}
              type="button"
              variant="outline"
            >
              Cancel
            </Button>
            <Button
              disabled={
                isSubmitting ||
                createSnapshot.isPending ||
                selectedInstance === ""
              }
              type="submit"
            >
              {createSnapshot.isPending ? "Taking snapshot…" : "Take snapshot"}
            </Button>
          </div>
        </form>
      </AlertDialogContent>
    </AlertDialog>
  )
}
