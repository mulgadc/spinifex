import type { Change, ResourceRecordSet } from "@aws-sdk/client-route-53"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Pencil, Plus, Trash2 } from "lucide-react"
import { useState } from "react"
import { useForm, useWatch } from "react-hook-form"

import { type DataColumn, DataTable } from "@/components/data-table"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { SelectField, selectOptions, TextField } from "@/components/form-fields"
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
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Textarea } from "@/components/ui/textarea"
import type { ActivationState } from "@/lib/route53-activation"
import { useChangeResourceRecordSets } from "@/mutations/route53"
import { route53RecordSetsQueryOptions } from "@/queries/route53"
import {
  decodeRecordName,
  EDITABLE_RECORD_TYPES,
  ensureTrailingDot,
  isEditableType,
  isZoneOwnedRecord,
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

interface RecordsTabProps {
  zoneId: string
  zoneName: string
  activationState: ActivationState | undefined
}

function recordKey(recordSet: ResourceRecordSet): string {
  return `${recordSet.Name ?? ""}|${recordSet.Type ?? ""}`
}

function recordLabel(recordSet: ResourceRecordSet): string {
  return `${decodeRecordName(recordSet.Name ?? "")} ${recordSet.Type ?? ""}`
}

function recordValues(recordSet: ResourceRecordSet): string[] {
  if (recordSet.AliasTarget) {
    return [`ALIAS ${recordSet.AliasTarget.DNSName ?? ""}`]
  }

  return (recordSet.ResourceRecords ?? []).map((r) => r.Value ?? "")
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

interface RecordActionsProps {
  recordSet: ResourceRecordSet
  zoneName: string
  onEdit: () => void
  onDelete: () => void
}

// The zone's own NS and SOA are not the customer's to change.
function RecordActions({
  recordSet,
  zoneName,
  onEdit,
  onDelete,
}: RecordActionsProps) {
  const label = recordLabel(recordSet)

  if (isZoneOwnedRecord(recordSet, zoneName)) {
    return (
      <span className="text-xs text-muted-foreground">Managed by the zone</span>
    )
  }

  const editable = !recordSet.AliasTarget && isEditableType(recordSet.Type)

  return (
    <div className="flex justify-end gap-1">
      {editable && (
        <Button
          aria-label={`Edit record ${label}`}
          onClick={onEdit}
          size="icon-sm"
          variant="ghost"
        >
          <Pencil />
        </Button>
      )}
      <Button
        aria-label={`Delete record ${label}`}
        onClick={onDelete}
        size="icon-sm"
        variant="ghost"
      >
        <Trash2 />
      </Button>
    </div>
  )
}

function recordColumns(
  zoneName: string,
  onEdit: (recordSet: ResourceRecordSet) => void,
  onDelete: (recordSet: ResourceRecordSet) => void,
): DataColumn<ResourceRecordSet>[] {
  return [
    {
      id: "name",
      header: "Name",
      cell: (recordSet) => (
        <span className="font-mono text-xs break-all">
          {decodeRecordName(recordSet.Name ?? "")}
        </span>
      ),
    },
    { id: "type", header: "Type", cell: (recordSet) => recordSet.Type },
    {
      id: "ttl",
      header: "TTL",
      hideBelow: "sm",
      cell: (recordSet) => recordSet.TTL ?? "—",
    },
    {
      id: "value",
      header: "Value",
      cell: (recordSet) => (
        <span className="font-mono text-xs break-all">
          {recordValues(recordSet).map((value) => (
            <span className="block" key={value}>
              {value}
            </span>
          ))}
        </span>
      ),
    },
    {
      id: "actions",
      header: "Actions",
      align: "end",
      cell: (recordSet) => (
        <RecordActions
          onDelete={() => {
            onDelete(recordSet)
          }}
          onEdit={() => {
            onEdit(recordSet)
          }}
          recordSet={recordSet}
          zoneName={zoneName}
        />
      ),
    },
  ]
}

export function RecordsTab({
  zoneId,
  zoneName,
  activationState,
}: RecordsTabProps) {
  const { data: recordSets } = useSuspenseQuery(
    route53RecordSetsQueryOptions(zoneId),
  )

  const changeMutation = useChangeResourceRecordSets()

  const [addOpen, setAddOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<ResourceRecordSet | undefined>()

  const [deleteTarget, setDeleteTarget] = useState<
    ResourceRecordSet | undefined
  >()

  const notServed =
    activationState !== undefined && activationState !== "ACTIVE"

  const dialogOpen = addOpen || editTarget !== undefined

  const submitChanges = async (changes: Change[]): Promise<boolean> => {
    try {
      await changeMutation.mutateAsync({ zoneId, changes })

      return true
    } catch {
      // surfaced via mutation.error
      return false
    }
  }

  const handleCreate = async (data: RecordSetFormData) => {
    const created = await submitChanges([
      { Action: "CREATE", ResourceRecordSet: toRecordSet(data, zoneName) },
    ])

    if (created) {
      setAddOpen(false)
    }
  }

  const handleEdit = async (data: RecordSetFormData) => {
    if (!editTarget) {
      return
    }

    const saved = await submitChanges(
      editChanges(editTarget, toRecordSet(data, zoneName)),
    )

    if (saved) {
      setEditTarget(undefined)
    }
  }

  const handleDelete = async () => {
    if (!deleteTarget) {
      return
    }

    await submitChanges([{ Action: "DELETE", ResourceRecordSet: deleteTarget }])
    setDeleteTarget(undefined)
  }

  const columns = recordColumns(
    zoneName,
    (recordSet) => {
      changeMutation.reset()
      setEditTarget(recordSet)
    },
    (recordSet) => {
      changeMutation.reset()
      setDeleteTarget(recordSet)
    },
  )

  return (
    <div className="flex flex-col gap-3">
      {notServed && (
        <div className="rounded-md border border-warning bg-warning/10 p-3 text-sm text-warning-foreground">
          Records are not served until the domain is verified. You can add them
          now so they answer the moment the name servers change.
        </div>
      )}

      {/* An open dialog shows the refusal itself, beside the values refused. */}
      {changeMutation.error && !dialogOpen && (
        <ErrorBanner
          error={changeMutation.error}
          msg="Failed to change records"
        />
      )}

      <div className="flex items-center justify-between">
        <p className="text-xs text-muted-foreground">
          {recordSets.length} record set{recordSets.length === 1 ? "" : "s"}
        </p>
        <Button
          onClick={() => {
            changeMutation.reset()
            setAddOpen(true)
          }}
          size="sm"
        >
          <Plus data-icon="inline-start" />
          Add record
        </Button>
      </div>

      <DataTable
        caption="Records"
        columns={columns}
        empty="No records."
        getRowId={recordKey}
        rows={recordSets}
      />

      {/* Mounted only while open so every session starts from blank defaults. */}
      {addOpen && (
        <RecordDialog
          error={changeMutation.error}
          isPending={changeMutation.isPending}
          mode="add"
          onOpenChange={setAddOpen}
          onSubmit={handleCreate}
          zoneName={zoneName}
        />
      )}

      {editTarget && (
        <RecordDialog
          error={changeMutation.error}
          initialValues={toFormValues(editTarget, zoneName)}
          isPending={changeMutation.isPending}
          mode="edit"
          onOpenChange={(open) => {
            if (!open) {
              setEditTarget(undefined)
            }
          }}
          onSubmit={handleEdit}
          zoneName={zoneName}
        />
      )}

      <DeleteConfirmationDialog
        description={
          deleteTarget
            ? `Delete the ${recordLabel(deleteTarget)} record set? It stops resolving once the change is in sync.`
            : ""
        }
        isPending={changeMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(undefined)
          }
        }}
        open={deleteTarget !== undefined}
        title="Delete record"
      />
    </div>
  )
}

interface RecordDialogProps {
  mode: "add" | "edit"
  zoneName: string
  initialValues?: RecordSetFormData
  isPending: boolean
  error: Error | null
  onOpenChange: (open: boolean) => void
  onSubmit: (data: RecordSetFormData) => Promise<void>
}

function RecordDialog({
  mode,
  zoneName,
  initialValues,
  isPending,
  error,
  onOpenChange,
  onSubmit,
}: RecordDialogProps) {
  const {
    control,
    register,
    handleSubmit,
    formState: { errors },
  } = useForm<RecordSetFormData>({
    resolver: zodResolver(recordSetSchema),
    defaultValues: initialValues ?? NEW_RECORD_DEFAULTS,
  })

  const type = useWatch({ control, name: "type" })
  const handleConfirm = handleSubmit(onSubmit)
  const isAdd = mode === "add"

  let actionLabel = isAdd ? "Add record" : "Save changes"

  if (isPending) {
    actionLabel = isAdd ? "Adding…" : "Saving…"
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open>
      <AlertDialogContent width="md">
        <AlertDialogHeader>
          <AlertDialogTitle>
            {isAdd ? "Add record" : "Edit record"}
          </AlertDialogTitle>
          <AlertDialogDescription>
            Records in {ensureTrailingDot(zoneName)}
          </AlertDialogDescription>
        </AlertDialogHeader>

        {error && <ErrorBanner error={error} msg="The change was refused" />}

        <form
          className="min-w-0 space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            void handleConfirm()
          }}
        >
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
        </form>

        <AlertDialogFooter>
          <AlertDialogCancel disabled={isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction disabled={isPending} onClick={handleConfirm}>
            {actionLabel}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
