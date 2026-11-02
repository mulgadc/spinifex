import type { Change, ResourceRecordSet } from "@aws-sdk/client-route-53"
import { zodResolver } from "@hookform/resolvers/zod"
import type { SubmitEvent } from "react"
import { type DeepPartial, useForm, useWatch } from "react-hook-form"

import {
  type CliCommand,
  CliCommandPanel,
  commandFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { SelectField, selectOptions, TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Textarea } from "@/components/ui/textarea"
import {
  EDITABLE_RECORD_TYPES,
  ensureTrailingDot,
  RECORD_VALUE_HINTS,
  type RecordSetFormData,
  recordSetSchema,
  sameName,
  toFormValues,
  toRecordSet,
} from "@/types/route53"

const DEFAULT_TTL_SECONDS = 300

const NEW_RECORD_DEFAULTS: RecordSetFormData = {
  name: "",
  type: "A",
  ttl: DEFAULT_TTL_SECONDS,
  values: "",
}

// editChanges is the batch that turns `original` into `updated`. A record set
// is keyed by name and type, so changing either is a delete and a create in one
// atomic batch; otherwise it is an UPSERT.
export function editChanges(
  original: ResourceRecordSet,
  updated: ResourceRecordSet,
): Change[] {
  const sameKey =
    sameName(original.Name ?? "", updated.Name ?? "") &&
    original.Type === updated.Type

  if (sameKey) {
    return [{ Action: "UPSERT", ResourceRecordSet: updated }]
  }

  return [
    { Action: "DELETE", ResourceRecordSet: original },
    { Action: "CREATE", ResourceRecordSet: updated },
  ]
}

interface RecordFormProps {
  zoneId: string
  zoneName: string
  // Set to edit that record set; unset adds a new one.
  original?: ResourceRecordSet
  isPending: boolean
  error: Error | null
  onClose: () => void
  onSubmit: (data: RecordSetFormData) => Promise<void>
}

export function RecordForm({
  zoneId,
  zoneName,
  original,
  isPending,
  error,
  onClose,
  onSubmit,
}: RecordFormProps) {
  const {
    control,
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
  } = useForm<RecordSetFormData>({
    resolver: zodResolver(recordSetSchema),
    defaultValues: original
      ? toFormValues(original, zoneName)
      : NEW_RECORD_DEFAULTS,
  })

  const values = useWatch({ control })
  const type = values.type ?? "A"
  const isAdd = original === undefined

  const submit = (event: SubmitEvent<HTMLFormElement>) => {
    event.stopPropagation()
    void handleSubmit(onSubmit)(event)
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={`Records in ${ensureTrailingDot(zoneName)}`}
      title={isAdd ? "Add record" : "Edit record"}
    >
      <form onSubmit={submit}>
        <FormLock className="space-y-4" locked={isPending}>
          <TextField
            description={`Leave blank for ${ensureTrailingDot(zoneName)} itself. The zone name is added for you.`}
            error={errors.name}
            id="record-name"
            label="Record name"
            placeholder="www"
            {...register("name")}
          />
          <SelectField
            control={control}
            id="record-type"
            label="Record type"
            name="type"
            options={selectOptions(EDITABLE_RECORD_TYPES)}
          />
          <TextField
            error={errors.ttl}
            id="record-ttl"
            label="TTL (seconds)"
            type="number"
            {...register("ttl", { valueAsNumber: true })}
          />
          <Field>
            <FieldTitle>
              <label htmlFor="record-values">Value</label>
            </FieldTitle>
            <Textarea
              aria-invalid={!!errors.values}
              id="record-values"
              rows={4}
              variant="code"
              {...register("values")}
            />
            <FieldDescription>{RECORD_VALUE_HINTS[type]}</FieldDescription>
            <FieldError errors={[errors.values]} />
          </Field>

          <CliCommandPanel
            commands={buildRecordCommands(values, zoneId, zoneName, original)}
          />
          <FormActions
            error={error}
            errorMsg="The change was refused"
            isPending={isPending}
            isSubmitting={isSubmitting}
            onCancel={onClose}
            pendingLabel={isAdd ? "Adding…" : "Saving…"}
            submitLabel={isAdd ? "Add record" : "Save changes"}
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

export function buildRecordCommands(
  values: DeepPartial<RecordSetFormData>,
  zoneId: string,
  zoneName: string,
  original: ResourceRecordSet | undefined,
): CliCommand[] {
  const updated = toRecordSet(
    {
      name: values.name ?? "",
      type: values.type ?? "A",
      ttl: Number.isFinite(values.ttl)
        ? (values.ttl ?? 0)
        : DEFAULT_TTL_SECONDS,
      values: values.values ?? "",
    },
    zoneName,
  )

  const changes: Change[] = original
    ? editChanges(original, updated)
    : [{ Action: "CREATE", ResourceRecordSet: updated }]

  return [
    {
      label: original ? "Change record set" : "Create record set",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws route53 change-resource-record-sets",
        },
        ...commandFlag("--hosted-zone-id", zoneId),
        ...commandFlag(
          "--change-batch",
          shellSingleQuote(JSON.stringify({ Changes: changes })),
        ),
      ],
    },
  ]
}
