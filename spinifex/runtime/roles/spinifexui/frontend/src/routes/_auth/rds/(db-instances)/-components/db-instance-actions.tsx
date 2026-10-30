import type { DBInstance } from "@aws-sdk/client-rds"
import { useNavigate } from "@tanstack/react-router"
import {
  Camera,
  ChevronDown,
  Maximize2,
  Pause,
  Pencil,
  Play,
  RotateCw,
  Trash2,
} from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { ButtonGroup } from "@/components/ui/button-group"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import type { PanelTab } from "@/lib/panel-search"
import {
  useRebootDBInstance,
  useStartDBInstance,
  useStopDBInstance,
} from "@/mutations/rds"
import {
  canDelete,
  canReboot,
  canSnapshot,
  canStart,
  canStop,
} from "@/types/rds"

import { DeleteDBInstanceDialog } from "./delete-db-instance-dialog"
import { ModifyDBInstanceDialog } from "./modify-db-instance-dialog"

type Dialog = "" | "stop" | "reboot" | "modify" | "delete"

interface DBInstanceActionsProps {
  instance: DBInstance
  layout: "auto" | "page"
  tab: PanelTab
  onDeleted: () => void
}

// Start, Stop and Reboot stay in view and disable by status; snapshot, modify
// and delete sit behind the Actions menu beside them.
export function DBInstanceActions({
  instance,
  layout,
  tab,
  onDeleted,
}: DBInstanceActionsProps) {
  const id = instance.DBInstanceIdentifier ?? ""
  const status = instance.DBInstanceStatus
  const navigate = useNavigate()
  const start = useStartDBInstance()
  const stop = useStopDBInstance()
  const reboot = useRebootDBInstance()
  const [dialog, setDialog] = useState<Dialog>("")
  const error = start.error ?? stop.error ?? reboot.error

  function closeDialog(open: boolean) {
    if (!open) {
      setDialog("")
    }
  }

  return (
    <>
      <div className="flex flex-wrap items-center justify-center gap-2">
        <ButtonGroup aria-label="Power">
          <Button
            disabled={!canStart(status) || start.isPending}
            onClick={() => {
              start.mutate(id)
            }}
            size="sm"
            variant="outline"
          >
            <Play data-icon="inline-start" />
            Start
          </Button>
          <Button
            disabled={!canStop(status) || stop.isPending}
            onClick={() => {
              setDialog("stop")
            }}
            size="sm"
            variant="outline"
          >
            <Pause data-icon="inline-start" />
            Stop
          </Button>
          <Button
            disabled={!canReboot(status) || reboot.isPending}
            onClick={() => {
              setDialog("reboot")
            }}
            size="sm"
            variant="outline"
          >
            <RotateCw data-icon="inline-start" />
            Reboot
          </Button>
        </ButtonGroup>
        <DropdownMenu>
          <DropdownMenuTrigger render={<Button size="sm" variant="outline" />}>
            Actions
            <ChevronDown data-icon="inline-end" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="min-w-48">
            {layout === "auto" && (
              <DropdownMenuItem
                onClick={() => {
                  void navigate({
                    to: "/rds/describe-db-instances/$id",
                    params: { id },
                    search: { tab },
                  })
                }}
              >
                <Maximize2 />
                Open database page
              </DropdownMenuItem>
            )}
            <DropdownMenuItem
              disabled={!canSnapshot(status)}
              onClick={() => {
                void navigate({
                  to: "/rds/describe-db-snapshots",
                  search: { create: true, instance: id },
                })
              }}
            >
              <Camera />
              Take snapshot
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => {
                setDialog("modify")
              }}
            >
              <Pencil />
              Modify
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              disabled={!canDelete(status)}
              onClick={() => {
                setDialog("delete")
              }}
              variant="destructive"
            >
              <Trash2 />
              Delete
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      {error && (
        <ErrorBanner
          error={error}
          msg="Failed to change the DB instance state."
        />
      )}

      <DeleteConfirmationDialog
        confirmLabel="Stop"
        description={`Stopping ${id} shuts its database down. Its storage is kept, and Start brings it back.`}
        isPending={stop.isPending}
        onConfirm={() => {
          stop.mutate(id, {
            onSettled: () => {
              setDialog("")
            },
          })
        }}
        onOpenChange={closeDialog}
        open={dialog === "stop"}
        pendingLabel="Stopping…"
        title="Stop DB instance?"
      />
      <DeleteConfirmationDialog
        confirmLabel="Reboot"
        description={`Rebooting ${id} restarts its database engine. Open connections are dropped.`}
        isPending={reboot.isPending}
        onConfirm={() => {
          reboot.mutate(id, {
            onSettled: () => {
              setDialog("")
            },
          })
        }}
        onOpenChange={closeDialog}
        open={dialog === "reboot"}
        pendingLabel="Rebooting…"
        title="Reboot DB instance?"
      />

      {/* Mounted per open so each form re-seeds from the current record; a
          dialog left mounted keeps the values it captured the first time. */}
      {dialog === "modify" && (
        <ModifyDBInstanceDialog
          instance={instance}
          onOpenChange={closeDialog}
          open={true}
        />
      )}
      <DeleteDBInstanceDialog
        dbInstanceIdentifier={id}
        deletionProtection={instance.DeletionProtection ?? false}
        onDeleted={onDeleted}
        onModify={() => {
          setDialog("modify")
        }}
        onOpenChange={closeDialog}
        open={dialog === "delete"}
      />
    </>
  )
}
