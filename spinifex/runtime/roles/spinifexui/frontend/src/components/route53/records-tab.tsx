import type { Change, ResourceRecordSet } from "@aws-sdk/client-route-53"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Plus } from "lucide-react"
import { useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
import { type DataColumn, DataTable } from "@/components/data-table"
import { ErrorBanner } from "@/components/error-banner"
import { editChanges, RecordForm } from "@/components/route53/record-form"
import { SelectionBar } from "@/components/selection-bar"
import { TruncatedText } from "@/components/table-cells"
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

function isEditableSet(set: ResourceRecordSet, zoneName: string): boolean {
  return (
    !isZoneOwnedRecord(set, zoneName) &&
    !set.AliasTarget &&
    isEditableType(set.Type)
  )
}

// An editable record's name is a button, so the edit pane opens from the
// keyboard as well as from a row click.
function recordColumns(
  zoneName: string,
  onOpen: (set: ResourceRecordSet) => void,
): DataColumn<ResourceRecordSet>[] {
  return [
    {
      id: "name",
      header: "Name",
      cell: (recordSet) => {
        const name = (
          <TruncatedText
            className="font-mono text-xs"
            text={decodeRecordName(recordSet.Name ?? "")}
          />
        )

        return isEditableSet(recordSet, zoneName) ? (
          <button
            aria-label={`Edit ${recordLabel(recordSet)}`}
            className="max-w-full rounded-sm text-left font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            onClick={() => {
              onOpen(recordSet)
            }}
            type="button"
          >
            {name}
          </button>
        ) : (
          name
        )
      },
    },
    { id: "type", header: "Type", cell: (recordSet) => recordSet.Type },
    {
      id: "value",
      header: "Value",
      cell: (recordSet) =>
        isZoneOwnedRecord(recordSet, zoneName) ? (
          <span className="flex flex-col">
            <TruncatedText
              className="font-mono text-xs text-muted-foreground"
              text={recordValues(recordSet).join(", ")}
            />
            <span className="text-2xs text-muted-foreground">
              Managed by the zone
            </span>
          </span>
        ) : (
          <TruncatedText
            className="font-mono text-xs"
            text={recordValues(recordSet).join(", ")}
          />
        ),
    },
    {
      id: "ttl",
      header: "TTL",
      hideBelow: "sm",
      cell: (recordSet) => recordSet.TTL ?? "—",
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

  const [selected, setSelected] = useState<Set<string>>(() => new Set())

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

  const byKey = new Map(recordSets.map((set) => [recordKey(set), set]))

  // The sets go in one batch, so the zone takes all of them or none.
  const deleteRecords = async (
    keys: readonly string[],
  ): Promise<ReadonlyMap<string, string>> => {
    const changes: Change[] = keys.flatMap((key) => {
      const set = byKey.get(key)

      return set ? [{ Action: "DELETE", ResourceRecordSet: set }] : []
    })

    try {
      await changeMutation.mutateAsync({ zoneId, changes })

      return new Map()
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error)

      return new Map(keys.map((key) => [key, message]))
    }
  }

  const editRecord = (set: ResourceRecordSet) => {
    changeMutation.reset()
    setEditTarget(set)
  }

  const columns = recordColumns(zoneName, editRecord)

  const actionable = (set: ResourceRecordSet) =>
    !isZoneOwnedRecord(set, zoneName)

  const openRecord = (key: string) => {
    const set = byKey.get(key)

    if (set && isEditableSet(set, zoneName)) {
      editRecord(set)
    }
  }

  const label = (key: string) => {
    const set = byKey.get(key)

    return set ? recordLabel(set) : key
  }

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

      {selected.size > 0 && (
        <SelectionBar
          count={selected.size}
          onClear={() => {
            setSelected(new Set())
          }}
        >
          <BulkDelete
            deleteMany={deleteRecords}
            description="They stop resolving once the change is in sync."
            ids={[...selected]}
            label={label}
            noun={["record set", "record sets"]}
            onDeleted={(keys) => {
              setSelected(
                (current) =>
                  new Set([...current].filter((key) => !keys.includes(key))),
              )
            }}
          />
        </SelectionBar>
      )}

      <DataTable
        activeId={editTarget ? recordKey(editTarget) : undefined}
        caption="Records"
        columns={columns}
        empty="No records."
        getRowId={recordKey}
        isRowActionable={actionable}
        onRowClick={openRecord}
        onSelectedChange={setSelected}
        rows={recordSets}
        selected={selected}
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
    </div>
  )
}
