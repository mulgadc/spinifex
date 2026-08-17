import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { Plus, Trash2 } from "lucide-react"
import { useState } from "react"
import { useForm, useWatch } from "react-hook-form"

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
  const { data: instancesData } = useQuery({
    ...rdsDBInstancesQueryOptions,
    enabled: dbInstanceIdentifier === undefined,
  })
  const [selectedInstance, setSelectedInstance] = useState(
    dbInstanceIdentifier ?? "",
  )

  // A snapshot is refused from anything but a settled instance, so an instance
  // mid-transition is not offered rather than offered and then refused.
  const instances = (instancesData?.DBInstances ?? []).filter((instance) =>
    canSnapshot(instance.DBInstanceStatus),
  )

  const {
    control,
    formState: { errors, isSubmitting },
    getValues,
    handleSubmit,
    register,
    reset,
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

  const values = useWatch({ control })
  const tags = values.tags ?? []

  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      createSnapshot.reset()
      reset()
    }
    onOpenChange(nextOpen)
  }

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
    handleOpenChange(false)
    onCreated?.()
  }

  return (
    <AlertDialog onOpenChange={handleOpenChange} open={open}>
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
              {instances.length === 0 ? (
                <p className="text-xs text-muted-foreground">
                  No DB instance is available or stopped, which is what a
                  snapshot is taken from.
                </p>
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

          <Field>
            <FieldTitle>Tags</FieldTitle>
            <div className="space-y-2">
              {tags.map((_, index) => (
                // oxlint-disable-next-line react/no-array-index-key -- form array with no stable id
                <div className="flex items-center gap-2" key={index}>
                  <Input placeholder="Key" {...register(`tags.${index}.key`)} />
                  <Input
                    placeholder="Value"
                    {...register(`tags.${index}.value`)}
                  />
                  <Button
                    onClick={() =>
                      setValue(
                        "tags",
                        getValues("tags").filter((__, i) => i !== index),
                      )
                    }
                    size="icon"
                    type="button"
                    variant="ghost"
                  >
                    <Trash2 className="size-3.5" />
                  </Button>
                </div>
              ))}
              <Button
                onClick={() =>
                  setValue("tags", [
                    ...getValues("tags"),
                    { key: "", value: "" },
                  ])
                }
                size="sm"
                type="button"
                variant="outline"
              >
                <Plus className="size-3.5" />
                Add tag
              </Button>
            </div>
          </Field>

          <p className="text-xs text-muted-foreground">
            A snapshot the engine could not be quiesced for is still taken and
            reported as crash consistent in the snapshot&apos;s events.
          </p>

          {createSnapshot.error && (
            <p className="text-sm text-destructive">
              {createSnapshot.error.message}
            </p>
          )}

          <div className="flex justify-end gap-2">
            <Button
              onClick={() => handleOpenChange(false)}
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
