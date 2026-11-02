import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  type CliCommand,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { PickerNoticeText, pickerNotice } from "@/components/picker-notice"
import { TagsFieldArray } from "@/components/tags-field-array"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
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

interface CreateDBSnapshotPageProps {
  // Fixed when the form is opened from an instance. Without it the form picks
  // the instance itself, as the snapshots list's Take snapshot does.
  dbInstanceIdentifier?: string
  // Called with the new snapshot's identifier once taken, or nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateDBSnapshotPage({
  dbInstanceIdentifier,
  onClose,
}: CreateDBSnapshotPageProps) {
  const createSnapshot = useCreateDBSnapshot()

  // Only the picker needs the list, so a form opened from an instance does not
  // fetch it at all.
  const instancesQuery = useQuery({
    ...rdsDBInstancesQueryOptions,
    enabled: dbInstanceIdentifier === undefined,
  })

  const [selectedInstance, setSelectedInstance] = useState(
    dbInstanceIdentifier ?? "",
  )

  const [instanceMissing, setInstanceMissing] = useState(false)

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
    setInstanceMissing(false)

    if (identifier) {
      setValue(
        "dbSnapshotIdentifier",
        suggestedIdentifier(identifier, "snapshot"),
        { shouldValidate: true },
      )
    }
  }

  const snapshotIdentifier = useWatch({ control, name: "dbSnapshotIdentifier" })

  const onSubmit = async (data: CreateDBSnapshotFormData) => {
    if (selectedInstance === "") {
      return
    }

    try {
      await createSnapshot.mutateAsync({
        ...data,
        dbInstanceIdentifier: selectedInstance,
      })
    } catch {
      // Left open so the refusal beside the button stays readable.
      return
    }

    onClose(data.dbSnapshotIdentifier)
  }

  // The instance sits outside the schema, so it is checked alongside it rather
  // than only once the name has passed.
  return (
    <form
      onSubmit={(event) => {
        setInstanceMissing(selectedInstance === "")
        void handleSubmit(onSubmit)(event)
      }}
    >
      <FormLock
        className="space-y-6"
        locked={createSnapshot.isPending || isSubmitting}
      >
        <p className="text-sm text-muted-foreground">
          The engine is held at a checkpoint while the snapshot is taken, so the
          instance reads as backing-up until it finishes.
        </p>

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
            {instanceMissing && (
              <FieldError>Select the DB instance to snapshot.</FieldError>
            )}
          </Field>
        )}

        <TextField
          description="Lowercase letters, digits and hyphens. The rds: namespace belongs to automated backups and cannot be used here."
          error={errors.dbSnapshotIdentifier}
          id="snapshot-identifier"
          label="Snapshot identifier"
          {...register("dbSnapshotIdentifier")}
        />

        <TagsFieldArray control={control} name="tags" />

        <p className="text-xs text-muted-foreground">
          A snapshot the engine could not be quiesced for is still taken and
          reported as crash consistent on the source instance&apos;s Backups
          tab. The snapshot&apos;s own events never carry it.
        </p>

        <CliCommandPanel
          commands={buildCreateSnapshotCommands(
            selectedInstance,
            snapshotIdentifier,
          )}
        />

        <FormActions
          error={createSnapshot.error}
          errorMsg="Failed to take the DB snapshot"
          isPending={createSnapshot.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Taking snapshot…"
          submitLabel="Take snapshot"
        />
      </FormLock>
    </form>
  )
}

function buildCreateSnapshotCommands(
  dbInstanceIdentifier: string,
  dbSnapshotIdentifier: string,
): CliCommand[] {
  return [
    {
      label: "Take DB Snapshot",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws rds create-db-snapshot",
        },
        ...commandFlag(
          "--db-instance-identifier",
          cliPlaceholder(dbInstanceIdentifier, "DBInstanceIdentifier"),
        ),
        ...commandFlag(
          "--db-snapshot-identifier",
          cliPlaceholder(dbSnapshotIdentifier, "DBSnapshotIdentifier"),
        ),
      ],
    },
  ]
}
