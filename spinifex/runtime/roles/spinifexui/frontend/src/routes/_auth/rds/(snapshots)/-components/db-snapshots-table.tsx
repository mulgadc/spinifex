import type { DBSnapshot } from "@aws-sdk/client-rds"
import { Link, useNavigate } from "@tanstack/react-router"

import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { canDeleteSnapshot, canRestoreSnapshot } from "@/types/rds"

interface DBSnapshotsTableProps {
  snapshots: DBSnapshot[]
  // The source instance, engine and storage columns, redundant on the
  // instance's own page.
  showSourceColumns: boolean
  onDelete: (dbSnapshotIdentifier: string) => void
}

export function DBSnapshotsTable({
  snapshots,
  showSourceColumns,
  onDelete,
}: DBSnapshotsTableProps) {
  const navigate = useNavigate()

  return (
    <div className="overflow-x-auto rounded-lg border bg-card">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="px-4 py-2 font-medium">Identifier</th>
            {showSourceColumns && (
              <th className="px-4 py-2 font-medium">DB instance</th>
            )}
            <th className="px-4 py-2 font-medium">Type</th>
            {showSourceColumns && (
              <>
                <th className="px-4 py-2 font-medium">Engine</th>
                <th className="px-4 py-2 font-medium">Storage</th>
              </>
            )}
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
                {showSourceColumns && (
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
                )}
                <td className="px-4 py-2">{snapshot.SnapshotType}</td>
                {showSourceColumns && (
                  <>
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
                  </>
                )}
                <td className="px-4 py-2">
                  <StateBadge state={snapshot.Status} />
                </td>
                <td className="px-4 py-2 font-mono text-xs">
                  {formatDateTime(snapshot.SnapshotCreateTime)}
                </td>
                <td className="space-x-2 px-4 py-2 text-right">
                  <Button
                    disabled={!canRestoreSnapshot(snapshot.Status)}
                    onClick={async () => {
                      await navigate({
                        to: "/rds/restore-db-instance-from-db-snapshot/$id",
                        params: { id },
                      })
                    }}
                    size="sm"
                    variant="outline"
                  >
                    Restore
                  </Button>
                  <Button
                    disabled={
                      !canDeleteSnapshot(snapshot.Status, snapshot.SnapshotType)
                    }
                    onClick={() => {
                      onDelete(id)
                    }}
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
  )
}
