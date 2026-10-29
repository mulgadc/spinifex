import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Camera, Pencil, Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import {
  useRebootDBInstance,
  useStartDBInstance,
  useStopDBInstance,
} from "@/mutations/rds"
import {
  rdsAutomatedBackupsQueryOptions,
  rdsDBInstanceQueryOptions,
  rdsEventsQueryOptions,
  rdsInstanceDBSnapshotsQueryOptions,
} from "@/queries/rds"
import {
  canDelete,
  canReboot,
  canSnapshot,
  canStart,
  canStop,
} from "@/types/rds"

import { CreateDBSnapshotDialog } from "../../(snapshots)/-components/create-db-snapshot-dialog"
import { DeleteDBSnapshotDialog } from "../../(snapshots)/-components/delete-db-snapshot-dialog"
import { RdsEventsPanel } from "../../-components/rds-events-panel"
import { RdsTagsTab } from "../../-components/rds-tags-tab"
import {
  BackupsTab,
  ConfigurationTab,
  ConnectivityTab,
} from "./db-instance-detail-tabs"
import { DeleteDBInstanceDialog } from "./delete-db-instance-dialog"
import { ModifyDBInstanceDialog } from "./modify-db-instance-dialog"

interface Props {
  dbInstanceIdentifier: string
}

interface InstanceActionsProps {
  dbInstanceIdentifier: string
  onDelete: () => void
  onModify: () => void
  onSnapshot: () => void
  rebootInstance: ReturnType<typeof useRebootDBInstance>
  startInstance: ReturnType<typeof useStartDBInstance>
  status: string | undefined
  stopInstance: ReturnType<typeof useStopDBInstance>
}

function InstanceActions({
  dbInstanceIdentifier,
  onDelete,
  onModify,
  onSnapshot,
  rebootInstance,
  startInstance,
  status,
  stopInstance,
}: InstanceActionsProps) {
  return (
    <div className="flex gap-2">
      <Button
        disabled={!canStart(status) || startInstance.isPending}
        onClick={() => {
          startInstance.mutate(dbInstanceIdentifier)
        }}
        size="sm"
        variant="outline"
      >
        Start
      </Button>
      <Button
        disabled={!canStop(status) || stopInstance.isPending}
        onClick={() => {
          stopInstance.mutate(dbInstanceIdentifier)
        }}
        size="sm"
        variant="outline"
      >
        Stop
      </Button>
      <Button
        disabled={!canReboot(status) || rebootInstance.isPending}
        onClick={() => {
          rebootInstance.mutate(dbInstanceIdentifier)
        }}
        size="sm"
        variant="outline"
      >
        Reboot
      </Button>
      <Button
        disabled={!canSnapshot(status)}
        onClick={onSnapshot}
        size="sm"
        variant="outline"
      >
        <Camera className="size-4" />
        Take Snapshot
      </Button>
      <Button onClick={onModify} size="sm" variant="outline">
        <Pencil className="size-4" />
        Modify
      </Button>
      <Button
        disabled={!canDelete(status)}
        onClick={onDelete}
        size="sm"
        variant="destructive"
      >
        <Trash2 className="size-4" />
        Delete
      </Button>
    </div>
  )
}

export function DBInstanceDetailPage({ dbInstanceIdentifier }: Props) {
  const navigate = useNavigate()

  const { data: instanceData } = useSuspenseQuery(
    rdsDBInstanceQueryOptions(dbInstanceIdentifier),
  )

  const instance = instanceData.DBInstances?.[0]
  const arn = instance?.DBInstanceArn ?? ""

  // Not suspense: the ARN is empty until the describe lands, and an empty one
  // is a request the tags API refuses.
  const { data: eventsData } = useSuspenseQuery(
    rdsEventsQueryOptions(dbInstanceIdentifier),
  )

  const { data: snapshotsData } = useSuspenseQuery(
    rdsInstanceDBSnapshotsQueryOptions(dbInstanceIdentifier),
  )

  const { data: automatedBackupsData } = useSuspenseQuery(
    rdsAutomatedBackupsQueryOptions(dbInstanceIdentifier),
  )

  const startInstance = useStartDBInstance()
  const stopInstance = useStopDBInstance()
  const rebootInstance = useRebootDBInstance()
  const [showDelete, setShowDelete] = useState(false)
  const [showModify, setShowModify] = useState(false)
  const [showSnapshot, setShowSnapshot] = useState(false)

  const [deleteSnapshotTarget, setDeleteSnapshotTarget] = useState<
    string | null
  >(null)

  if (!instance?.DBInstanceIdentifier) {
    return (
      <>
        <BackLink to="/rds/describe-db-instances">Back to databases</BackLink>
        <p className="text-muted-foreground">DB instance not found.</p>
      </>
    )
  }

  const status = instance.DBInstanceStatus
  const events = eventsData.Events ?? []

  const lifecycleError =
    startInstance.error ?? stopInstance.error ?? rebootInstance.error

  const snapshots = snapshotsData.DBSnapshots ?? []
  const automatedBackup = automatedBackupsData.DBInstanceAutomatedBackups?.[0]

  return (
    <>
      <BackLink to="/rds/describe-db-instances">Back to databases</BackLink>

      {lifecycleError && (
        <ErrorBanner
          error={lifecycleError}
          msg="Failed to change the DB instance state."
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <InstanceActions
              dbInstanceIdentifier={dbInstanceIdentifier}
              onDelete={() => {
                setShowDelete(true)
              }}
              onModify={() => {
                setShowModify(true)
              }}
              onSnapshot={() => {
                setShowSnapshot(true)
              }}
              rebootInstance={rebootInstance}
              startInstance={startInstance}
              status={status}
              stopInstance={stopInstance}
            />
          }
          subtitle="DB Instance"
          title={dbInstanceIdentifier}
        />

        <div className="flex items-center gap-2">
          <StateBadge state={status} />
          {instance.StatusInfos?.map((info) => (
            <span
              className="text-xs text-tactical-amber"
              key={`${info.StatusType}-${info.Status}`}
            >
              {info.StatusType}: {info.Message ?? info.Status}
            </span>
          ))}
        </div>

        <Tabs defaultValue="connectivity">
          <TabsList>
            <TabsTab value="connectivity">Connectivity</TabsTab>
            <TabsTab value="configuration">Configuration</TabsTab>
            <TabsTab value="backups">Backups</TabsTab>
            <TabsTab value="tags">Tags</TabsTab>
            <TabsTab value="events">Events</TabsTab>
          </TabsList>

          <TabsPanel value="connectivity">
            <ConnectivityTab instance={instance} />
          </TabsPanel>

          <TabsPanel value="configuration">
            <ConfigurationTab instance={instance} />
          </TabsPanel>

          <TabsPanel value="backups">
            <BackupsTab
              automatedBackup={automatedBackup}
              events={events}
              instance={instance}
              onDeleteSnapshot={setDeleteSnapshotTarget}
              snapshots={snapshots}
            />
          </TabsPanel>

          <TabsPanel value="tags">
            <RdsTagsTab arn={arn} />
          </TabsPanel>

          <TabsPanel value="events">
            <RdsEventsPanel events={events} />
          </TabsPanel>
        </Tabs>
      </div>

      {/* Mounted per open so each form re-seeds from the current record; a
          dialog left mounted keeps the values it captured the first time. */}
      {showModify && (
        <ModifyDBInstanceDialog
          instance={instance}
          onOpenChange={setShowModify}
          open={true}
        />
      )}

      {showSnapshot && (
        <CreateDBSnapshotDialog
          dbInstanceIdentifier={dbInstanceIdentifier}
          onOpenChange={setShowSnapshot}
          open={true}
        />
      )}

      {deleteSnapshotTarget && (
        <DeleteDBSnapshotDialog
          dbSnapshotIdentifier={deleteSnapshotTarget}
          onOpenChange={(open) => {
            if (!open) {
              setDeleteSnapshotTarget(null)
            }
          }}
          open={true}
        />
      )}

      <DeleteDBInstanceDialog
        dbInstanceIdentifier={dbInstanceIdentifier}
        deletionProtection={instance.DeletionProtection ?? false}
        onDeleted={async () => {
          await navigate({ to: "/rds/describe-db-instances" })
        }}
        onModify={() => {
          setShowDelete(false)
          setShowModify(true)
        }}
        onOpenChange={setShowDelete}
        open={showDelete}
      />
    </>
  )
}
