import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { useState } from "react"

import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { rdsDBSnapshotsQueryOptions } from "@/queries/rds"
import {
  canDeleteSnapshot,
  canRestoreSnapshot,
  SNAPSHOT_TYPE_AUTOMATED,
  SNAPSHOT_TYPE_MANUAL,
} from "@/types/rds"

import { RdsSectionNav } from "../../-components/rds-section-nav"
import { CreateDBSnapshotDialog } from "./create-db-snapshot-dialog"
import { DeleteDBSnapshotDialog } from "./delete-db-snapshot-dialog"

const TYPE_FILTERS = [
  { value: "", label: "All types" },
  { value: SNAPSHOT_TYPE_MANUAL, label: "Manual" },
  { value: SNAPSHOT_TYPE_AUTOMATED, label: "Automated" },
] as const

export function DBSnapshotsListPage() {
  const navigate = useNavigate()
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
          <Button onClick={() => setShowCreate(true)}>Take snapshot</Button>
        }
        title="Snapshots"
      />

      <RdsSectionNav />

      <div className="mb-4 flex items-center gap-2">
        {TYPE_FILTERS.map((filter) => (
          <Button
            key={filter.value}
            onClick={() => setTypeFilter(filter.value)}
            size="sm"
            variant={typeFilter === filter.value ? "default" : "outline"}
          >
            {filter.label}
          </Button>
        ))}
      </div>

      {snapshots.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Identifier</th>
                <th className="px-4 py-2 font-medium">DB instance</th>
                <th className="px-4 py-2 font-medium">Type</th>
                <th className="px-4 py-2 font-medium">Engine</th>
                <th className="px-4 py-2 font-medium">Storage</th>
                <th className="px-4 py-2 font-medium">Status</th>
                <th className="px-4 py-2 font-medium">Created</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {snapshots.map((snapshot) => {
                const id = snapshot.DBSnapshotIdentifier
                if (!id) {
                  return null
                }
                const sourceId = snapshot.DBInstanceIdentifier ?? ""
                return (
                  <tr className="border-b last:border-0" key={id}>
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        params={{ id }}
                        to="/rds/describe-db-snapshots/$id"
                      >
                        {id}
                      </Link>
                    </td>
                    <td className="px-4 py-2">
                      {sourceId === "" ? (
                        "—"
                      ) : (
                        <Link
                          className="text-primary hover:underline"
                          params={{ id: sourceId }}
                          to="/rds/describe-db-instances/$id"
                        >
                          {sourceId}
                        </Link>
                      )}
                    </td>
                    <td className="px-4 py-2">{snapshot.SnapshotType}</td>
                    <td className="px-4 py-2">
                      {[snapshot.Engine, snapshot.EngineVersion]
                        .filter(Boolean)
                        .join(" ")}
                    </td>
                    <td className="px-4 py-2">
                      {snapshot.AllocatedStorage
                        ? `${snapshot.AllocatedStorage} GiB`
                        : "—"}
                    </td>
                    <td className="px-4 py-2">
                      <StateBadge state={snapshot.Status} />
                    </td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {formatDateTime(snapshot.SnapshotCreateTime)}
                    </td>
                    <td className="space-x-2 px-4 py-2 text-right">
                      <Button
                        disabled={!canRestoreSnapshot(snapshot.Status)}
                        onClick={async () =>
                          await navigate({
                            to: "/rds/restore-db-instance-from-db-snapshot/$id",
                            params: { id },
                          })
                        }
                        size="sm"
                        variant="outline"
                      >
                        Restore
                      </Button>
                      <Button
                        disabled={
                          !canDeleteSnapshot(
                            snapshot.Status,
                            snapshot.SnapshotType,
                          )
                        }
                        onClick={() => setDeleteTarget(id)}
                        size="sm"
                        variant="destructive"
                      >
                        Delete
                      </Button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
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
          onOpenChange={(open) => !open && setDeleteTarget(null)}
          open={true}
        />
      )}
    </>
  )
}
