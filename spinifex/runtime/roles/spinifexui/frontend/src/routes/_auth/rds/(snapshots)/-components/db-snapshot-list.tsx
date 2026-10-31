import type { DBSnapshot } from "@aws-sdk/client-rds"
import { Link } from "@tanstack/react-router"
import { useState } from "react"

import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { canDeleteSnapshot, canRestoreSnapshot } from "@/types/rds"

import { DeleteDBSnapshotDialog } from "./delete-db-snapshot-dialog"

// An instance's snapshots, stacked so they fit a side panel's narrow column.
// Each restores or deletes where it stands.
export function DBSnapshotList({ snapshots }: { snapshots: DBSnapshot[] }) {
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)

  if (snapshots.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No snapshots of this instance.
      </p>
    )
  }

  return (
    <>
      <ul aria-label="Snapshots" className="divide-y text-sm">
        {snapshots.map((snapshot) => {
          const id = snapshot.DBSnapshotIdentifier

          if (!id) {
            return null
          }

          const restorable = canRestoreSnapshot(snapshot.Status)

          return (
            <li className="flex flex-col gap-1.5 py-2" key={id}>
              <div className="flex items-center justify-between gap-2">
                <Link
                  className="min-w-0 truncate font-mono text-xs text-primary hover:underline"
                  params={{ id }}
                  to="/rds/describe-db-snapshots/$id"
                >
                  {id}
                </Link>
                <StateBadge state={snapshot.Status} />
              </div>
              <div className="flex items-center justify-between gap-2">
                <span className="text-xs text-muted-foreground">
                  {snapshot.SnapshotType} ·{" "}
                  {formatDateTime(snapshot.SnapshotCreateTime)}
                </span>
                <span className="flex gap-1">
                  {restorable ? (
                    <Link
                      className={buttonVariants({
                        size: "xs",
                        variant: "outline",
                      })}
                      params={{ id }}
                      to="/rds/restore-db-instance-from-db-snapshot/$id"
                    >
                      Restore
                    </Link>
                  ) : (
                    <Button disabled size="xs" variant="outline">
                      Restore
                    </Button>
                  )}
                  <Button
                    disabled={
                      !canDeleteSnapshot(snapshot.Status, snapshot.SnapshotType)
                    }
                    onClick={() => {
                      setDeleteTarget(id)
                    }}
                    size="xs"
                    variant="destructive"
                  >
                    Delete
                  </Button>
                </span>
              </div>
            </li>
          )
        })}
      </ul>

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
