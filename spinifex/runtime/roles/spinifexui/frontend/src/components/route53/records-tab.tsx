import type { Change, ResourceRecordSet } from "@aws-sdk/client-route-53"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Pencil, Plus, Trash2 } from "lucide-react"
import { useState } from "react"

import { type DataColumn, DataTable } from "@/components/data-table"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { editChanges, RecordForm } from "@/components/route53/record-form"
import { Button } from "@/components/ui/button"
import type { ActivationState } from "@/lib/route53-activation"
import { useChangeResourceRecordSets } from "@/mutations/route53"
import { route53RecordSetsQueryOptions } from "@/queries/route53"
import {
  decodeRecordName,
  isEditableType,
  isZoneOwnedRecord,
  type RecordSetFormData,
  toRecordSet,
} from "@/types/route53"

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

  const formOpen = addOpen || editTarget !== undefined

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

      {/* An open form shows the refusal itself, beside the values refused. */}
      {changeMutation.error && !formOpen && (
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
        <RecordForm
          error={changeMutation.error}
          isPending={changeMutation.isPending}
          onClose={() => {
            changeMutation.reset()
            setAddOpen(false)
          }}
          onSubmit={handleCreate}
          zoneId={zoneId}
          zoneName={zoneName}
        />
      )}

      {editTarget && (
        <RecordForm
          error={changeMutation.error}
          isPending={changeMutation.isPending}
          key={recordKey(editTarget)}
          onClose={() => {
            changeMutation.reset()
            setEditTarget(undefined)
          }}
          onSubmit={handleEdit}
          original={editTarget}
          zoneId={zoneId}
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
