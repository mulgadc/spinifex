import type { Change, ResourceRecordSet } from "@aws-sdk/client-route-53"
import { zodResolver } from "@hookform/resolvers/zod"
import type { SubmitEvent } from "react"
import {
  type Control,
  Controller,
  type DeepPartial,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  type CliCommand,
  CliCommandPanel,
  commandFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import {
  FieldLabel,
  SelectField,
  selectOptions,
  TextField,
} from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { InfoList } from "@/components/info-tip"
import { Field, FieldDescription, FieldError } from "@/components/ui/field"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Textarea } from "@/components/ui/textarea"
import {
  DEFAULT_MX_PRIORITY,
  DEFAULT_TTL_SECONDS,
  EDITABLE_RECORD_TYPES,
  ensureTrailingDot,
  RECORD_TYPE_HELP,
  RECORD_VALUE_HINTS,
  RECORD_VALUE_LABELS,
  type RecordSetFormData,
  recordSetSchema,
  sameName,
  TTL_OPTIONS,
  toFormValues,
  toRecordSet,
} from "@/types/route53"

const NEW_RECORD_DEFAULTS: RecordSetFormData = {
  name: "",
  type: "A",
  ttl: DEFAULT_TTL_SECONDS,
  priority: DEFAULT_MX_PRIORITY,
  values: "",
}

// The preset TTLs, plus a record's own value when it is not one of them, so
// opening and saving a record never rewrites its TTL.
export function ttlOptions(current: number | undefined) {
  const custom =
    current !== undefined &&
    Number.isFinite(current) &&
    !TTL_OPTIONS.some((option) => option.value === current)

  const options = custom
    ? [...TTL_OPTIONS, { value: current, label: `${current} seconds` }]
    : TTL_OPTIONS

  return options.map((option) => ({
    value: String(option.value),
    label: option.label,
  }))
}

function TtlField({
  control,
  current,
}: {
  control: Control<RecordSetFormData>
  current: number | undefined
}) {
  const options = ttlOptions(current)

  return (
    <Controller
      control={control}
      name="ttl"
      render={({ field, fieldState }) => (
        <Field>
          <FieldLabel id="record-ttl" label="TTL" />
          <Select
            items={options}
            onValueChange={(next) => {
              field.onChange(Number(next ?? DEFAULT_TTL_SECONDS))
            }}
            value={String(field.value)}
          >
            <SelectTrigger
              aria-invalid={!!fieldState.error}
              className="w-full"
              id="record-ttl"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {options.map((option) => (
                <SelectItem key={option.value} value={option.value}>
                  {option.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <FieldError errors={[fieldState.error]} />
        </Field>
      )}
    />
  )
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
            info={<InfoList items={RECORD_TYPE_HELP} />}
            label="Record type"
            name="type"
            options={selectOptions(EDITABLE_RECORD_TYPES)}
          />
          {type === "MX" && (
            <TextField
              description="Lower is tried first."
              error={errors.priority}
              id="record-priority"
              label="Priority"
              type="number"
              {...register("priority", { valueAsNumber: true })}
            />
          )}
          <Field>
            <FieldLabel id="record-values" label={RECORD_VALUE_LABELS[type]} />
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
          <TtlField control={control} current={original?.TTL} />

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
      priority: Number.isFinite(values.priority)
        ? (values.priority ?? DEFAULT_MX_PRIORITY)
        : DEFAULT_MX_PRIORITY,
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
