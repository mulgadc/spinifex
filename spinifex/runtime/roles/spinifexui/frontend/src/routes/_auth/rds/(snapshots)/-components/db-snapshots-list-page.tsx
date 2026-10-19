import { useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"

import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { rdsDBSnapshotsQueryOptions } from "@/queries/rds"
import { SNAPSHOT_TYPE_AUTOMATED, SNAPSHOT_TYPE_MANUAL } from "@/types/rds"

import { CreateDBSnapshotDialog } from "./create-db-snapshot-dialog"
import { DBSnapshotsTable } from "./db-snapshots-table"
import { DeleteDBSnapshotDialog } from "./delete-db-snapshot-dialog"

const TYPE_FILTERS = [
  { value: "", label: "All types" },
  { value: SNAPSHOT_TYPE_MANUAL, label: "Manual" },
  { value: SNAPSHOT_TYPE_AUTOMATED, label: "Automated" },
] as const

export function DBSnapshotsListPage() {
  const { data } = useSuspenseQuery(rdsDBSnapshotsQueryOptions)
  const [typeFilter, setTypeFilter] = useState<string>("")
  const [showCreate, setShowCreate] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)

  const snapshots = (data.DBSnapshots ?? []).filter(
    (snapshot) => typeFilter === "" || snapshot.SnapshotType === typeFilter,
  )

  return (
    <>
      <PageHeading
        actions={
          <Button
            onClick={() => {
              setShowCreate(true)
            }}
          >
            Take Snapshot
          </Button>
        }
        title="Snapshots"
      />

      <div className="mb-4 flex items-center gap-2">
        {TYPE_FILTERS.map((filter) => (
          <Button
            key={filter.value}
            onClick={() => {
              setTypeFilter(filter.value)
            }}
            size="sm"
            variant={typeFilter === filter.value ? "default" : "outline"}
          >
            {filter.label}
          </Button>
        ))}
      </div>

      {snapshots.length > 0 ? (
        <DBSnapshotsTable
          onDelete={setDeleteTarget}
          showSourceColumns={true}
          snapshots={snapshots}
        />
      ) : (
        <p className="text-muted-foreground">No DB snapshots found.</p>
      )}

      <p className="mt-4 text-xs text-muted-foreground">
        Automated backups live in the reserved rds: namespace and cannot be
        deleted by hand — the instance&apos;s backup retention is what removes
        them.
      </p>

      {showCreate && (
        <CreateDBSnapshotDialog onOpenChange={setShowCreate} open={true} />
      )}

      {deleteTarget && (
        <DeleteDBSnapshotDialog
          dbSnapshotIdentifier={deleteTarget}
          onOpenChange={(open) => {
            if (!open) {
              setDeleteTarget(null)
            }
          }}
          open={true}
        />
      )}
    </>
  )
}
