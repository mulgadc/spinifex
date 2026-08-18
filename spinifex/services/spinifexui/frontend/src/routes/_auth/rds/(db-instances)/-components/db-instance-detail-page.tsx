import type { DBInstance, Event } from "@aws-sdk/client-rds"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { Camera, Pencil, Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { formatDateTime } from "@/lib/utils"
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
  canDeleteSnapshot,
  canReboot,
  canRestoreSnapshot,
  canSnapshot,
  canStart,
  canStop,
  SNAPSHOT_TYPE_AUTOMATED,
} from "@/types/rds"

import { CreateDBSnapshotDialog } from "../../(snapshots)/-components/create-db-snapshot-dialog"
import { DeleteDBSnapshotDialog } from "../../(snapshots)/-components/delete-db-snapshot-dialog"
import { RdsEventsPanel } from "../../-components/rds-events-panel"
import { RdsTagsTab } from "../../-components/rds-tags-tab"
import { DeleteDBInstanceDialog } from "./delete-db-instance-dialog"
import { ModifyDBInstanceDialog } from "./modify-db-instance-dialog"

interface Props {
  dbInstanceIdentifier: string
}

function hasCategory(event: Event, category: string): boolean {
  return event.EventCategories?.includes(category) ?? false
}

function latestEvent(
  events: Event[],
  predicate: (event: Event) => boolean,
): Event | undefined {
  return events
    .filter(predicate)
    .toSorted((a, b) => (a.Date?.getTime() ?? 0) - (b.Date?.getTime() ?? 0))
    .at(-1)
}

// The connection command for the engine, ready to paste. TLS is enforced on
// the serving side, so both forms ask for it rather than falling back silently.
function buildConnectCommand(instance: DBInstance): CliCommand[] {
  const address = instance.Endpoint?.Address
  if (!address) {
    return []
  }
  const port = instance.Endpoint?.Port ?? instance.DbInstancePort
  const user = instance.MasterUsername ?? "<MasterUsername>"
  const database = instance.DBName ?? user

  if (instance.Engine === "postgres") {
    return [
      {
        label: "Connect",
        parts: [
          { type: "bin", value: "psql" },
          {
            type: "value",
            value: ` "host=${address} port=${port} dbname=${database} user=${user} sslmode=require"`,
          },
        ],
      },
    ]
  }
  return [
    {
      label: "Connect",
      parts: [
        { type: "bin", value: "mariadb" },
        { type: "flag", value: " --host" },
        { type: "value", value: `=${address}` },
        { type: "flag", value: " --port" },
        { type: "value", value: `=${port}` },
        { type: "flag", value: " --user" },
        { type: "value", value: `=${user}` },
        { type: "flag", value: " --ssl" },
        { type: "value", value: ` ${database}` },
      ],
    },
  ]
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

  // The record's backup failure counters are internal and never projected onto
  // a DB instance, so the API-visible signal is the failure event itself.
  const lastBackupFailure = latestEvent(
    events,
    (event) => hasCategory(event, "backup") && hasCategory(event, "failure"),
  )
  // A quiesce that could not be taken or released is recorded as a backup
  // notification. It is the only signal that a snapshot is crash consistent,
  // since the record's flag is never projected onto the snapshot.
  const lastBackupWarning = latestEvent(
    events,
    (event) =>
      hasCategory(event, "backup") && hasCategory(event, "notification"),
  )
  const snapshots = snapshotsData.DBSnapshots ?? []
  // The success of an automated backup is only ever evented on the snapshot's
  // own ring, so the newest automated snapshot is the instance-side answer.
  const lastBackup = snapshots
    .filter((s) => s.SnapshotType === SNAPSHOT_TYPE_AUTOMATED)
    .toSorted(
      (a, b) =>
        (a.SnapshotCreateTime?.getTime() ?? 0) -
        (b.SnapshotCreateTime?.getTime() ?? 0),
    )
    .at(-1)
  const automatedBackup = automatedBackupsData.DBInstanceAutomatedBackups?.[0]
  const pending = instance.PendingModifiedValues
  const hasPending =
    pending?.AllocatedStorage !== undefined ||
    pending?.DBInstanceClass !== undefined

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
            <div className="flex gap-2">
              <Button
                disabled={!canStart(status) || startInstance.isPending}
                onClick={() => startInstance.mutate(dbInstanceIdentifier)}
                size="sm"
                variant="outline"
              >
                Start
              </Button>
              <Button
                disabled={!canStop(status) || stopInstance.isPending}
                onClick={() => stopInstance.mutate(dbInstanceIdentifier)}
                size="sm"
                variant="outline"
              >
                Stop
              </Button>
              <Button
                disabled={!canReboot(status) || rebootInstance.isPending}
                onClick={() => rebootInstance.mutate(dbInstanceIdentifier)}
                size="sm"
                variant="outline"
              >
                Reboot
              </Button>
              <Button
                disabled={!canSnapshot(status)}
                onClick={() => setShowSnapshot(true)}
                size="sm"
                variant="outline"
              >
                <Camera className="size-4" />
                Take snapshot
              </Button>
              <Button
                onClick={() => setShowModify(true)}
                size="sm"
                variant="outline"
              >
                <Pencil className="size-4" />
                Modify
              </Button>
              <Button
                disabled={!canDelete(status)}
                onClick={() => setShowDelete(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
            </div>
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
            <div className="space-y-4">
              <DetailCard>
                <DetailCard.Header>Endpoint</DetailCard.Header>
                <DetailCard.Content>
                  <DetailRow
                    label="Address"
                    value={instance.Endpoint?.Address}
                  />
                  <DetailRow
                    label="Port"
                    value={(
                      instance.Endpoint?.Port ?? instance.DbInstancePort
                    )?.toString()}
                  />
                  <DetailRow
                    label="VPC"
                    value={instance.DBSubnetGroup?.VpcId}
                  />
                  <DetailRow
                    label="DB subnet group"
                    value={instance.DBSubnetGroup?.DBSubnetGroupName}
                  />
                  <DetailRow
                    label="Security groups"
                    value={instance.VpcSecurityGroups?.map(
                      (g) => g.VpcSecurityGroupId,
                    ).join(", ")}
                  />
                  <DetailRow
                    label="Publicly accessible"
                    value="No — private VPC address"
                  />
                </DetailCard.Content>
              </DetailCard>

              <CliCommandPanel commands={buildConnectCommand(instance)} />
            </div>
          </TabsPanel>

          <TabsPanel value="configuration">
            <div className="space-y-4">
              <DetailCard>
                <DetailCard.Header>Configuration</DetailCard.Header>
                <DetailCard.Content>
                  <DetailRow label="Engine" value={instance.Engine} />
                  <DetailRow label="Version" value={instance.EngineVersion} />
                  <DetailRow
                    label="Instance class"
                    value={instance.DBInstanceClass}
                  />
                  <DetailRow
                    label="Allocated storage"
                    value={
                      instance.AllocatedStorage
                        ? `${instance.AllocatedStorage} GiB`
                        : undefined
                    }
                  />
                  <DetailRow
                    label="Storage type"
                    value={instance.StorageType}
                  />
                  <DetailRow
                    label="Encryption"
                    value={
                      instance.StorageEncrypted
                        ? "Encrypted — always on"
                        : "Not encrypted"
                    }
                  />
                  <DetailRow
                    label="Parameter group"
                    value={instance.DBParameterGroups?.map(
                      (g) =>
                        `${g.DBParameterGroupName} (${g.ParameterApplyStatus})`,
                    ).join(", ")}
                  />
                  <DetailRow
                    label="Master username"
                    value={instance.MasterUsername}
                  />
                  <DetailRow label="Initial database" value={instance.DBName} />
                  <DetailRow
                    label="Deletion protection"
                    value={instance.DeletionProtection ? "On" : "Off"}
                  />
                  <DetailRow
                    label="Created"
                    value={formatDateTime(instance.InstanceCreateTime)}
                  />
                </DetailCard.Content>
              </DetailCard>

              {hasPending && (
                <DetailCard>
                  <DetailCard.Header>Pending changes</DetailCard.Header>
                  <DetailCard.Content>
                    <DetailRow
                      label="Instance class"
                      value={pending?.DBInstanceClass}
                    />
                    <DetailRow
                      label="Allocated storage"
                      value={
                        pending?.AllocatedStorage
                          ? `${pending.AllocatedStorage} GiB`
                          : undefined
                      }
                    />
                  </DetailCard.Content>
                </DetailCard>
              )}
            </div>
          </TabsPanel>

          <TabsPanel value="backups">
            <div className="space-y-4">
              {lastBackupFailure && (
                <div
                  className="rounded-md border border-tactical-amber/40 bg-tactical-amber/5 p-4 text-sm"
                  role="alert"
                >
                  <p className="font-medium">An automated backup failed</p>
                  <p className="mt-1 text-xs text-muted-foreground">
                    {lastBackupFailure.Message} (
                    {formatDateTime(lastBackupFailure.Date)})
                  </p>
                </div>
              )}

              {lastBackupWarning && (
                <div
                  className="rounded-md border border-tactical-amber/40 bg-tactical-amber/5 p-4 text-sm"
                  role="alert"
                >
                  <p className="font-medium">A backup raised a warning</p>
                  <p className="mt-1 text-xs text-muted-foreground">
                    {lastBackupWarning.Message} (
                    {formatDateTime(lastBackupWarning.Date)})
                  </p>
                </div>
              )}

              <DetailCard>
                <DetailCard.Header>Automated backups</DetailCard.Header>
                <DetailCard.Content>
                  <DetailRow
                    label="Retention"
                    value={
                      instance.BackupRetentionPeriod === 0
                        ? "Disabled"
                        : `${instance.BackupRetentionPeriod} days`
                    }
                  />
                  <DetailRow
                    label="Status"
                    value={
                      automatedBackup?.Status ??
                      "None — automated backups are off"
                    }
                  />
                  <DetailRow
                    label="Preferred backup window"
                    value={instance.PreferredBackupWindow}
                  />
                  <DetailRow
                    label="Preferred maintenance window"
                    value={instance.PreferredMaintenanceWindow}
                  />
                  <DetailRow
                    label="Last backup"
                    value={
                      lastBackup?.SnapshotCreateTime &&
                      formatDateTime(lastBackup.SnapshotCreateTime)
                    }
                  />
                </DetailCard.Content>
              </DetailCard>

              <div className="space-y-2">
                <h3 className="text-sm font-medium">Snapshots</h3>
                {snapshots.length > 0 ? (
                  <div className="overflow-x-auto rounded-lg border bg-card">
                    <table className="w-full text-sm">
                      <thead>
                        <tr className="border-b text-left text-muted-foreground">
                          <th className="px-4 py-2 font-medium">Identifier</th>
                          <th className="px-4 py-2 font-medium">Type</th>
                          <th className="px-4 py-2 font-medium">Status</th>
                          <th className="px-4 py-2 font-medium">Created</th>
                          <th className="px-4 py-2 font-medium">
                            <span className="sr-only">Actions</span>
                          </th>
                        </tr>
                      </thead>
                      <tbody>
                        {snapshots.map((snapshot) => {
                          const snapshotId = snapshot.DBSnapshotIdentifier
                          if (!snapshotId) {
                            return null
                          }
                          return (
                            <tr
                              className="border-b last:border-0"
                              key={snapshotId}
                            >
                              <td className="px-4 py-2 font-medium">
                                <Link
                                  className="text-primary hover:underline"
                                  params={{ id: snapshotId }}
                                  to="/rds/describe-db-snapshots/$id"
                                >
                                  {snapshotId}
                                </Link>
                              </td>
                              <td className="px-4 py-2">
                                {snapshot.SnapshotType}
                              </td>
                              <td className="px-4 py-2">
                                <StateBadge state={snapshot.Status} />
                              </td>
                              <td className="px-4 py-2 font-mono text-xs">
                                {formatDateTime(snapshot.SnapshotCreateTime)}
                              </td>
                              <td className="space-x-2 px-4 py-2 text-right">
                                <Button
                                  disabled={
                                    !canRestoreSnapshot(snapshot.Status)
                                  }
                                  onClick={async () =>
                                    await navigate({
                                      to: "/rds/restore-db-instance-from-db-snapshot/$id",
                                      params: { id: snapshotId },
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
                                  onClick={() =>
                                    setDeleteSnapshotTarget(snapshotId)
                                  }
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
                  <p className="text-muted-foreground">
                    No snapshots of this instance.
                  </p>
                )}
              </div>

              <p className="text-xs text-muted-foreground">
                Automated backups are daily snapshots taken in the backup
                window, deleted by retention rather than by hand. There is no
                point-in-time restore.
              </p>
            </div>
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
          onOpenChange={(open) => !open && setDeleteSnapshotTarget(null)}
          open={true}
        />
      )}

      <DeleteDBInstanceDialog
        dbInstanceIdentifier={dbInstanceIdentifier}
        deletionProtection={instance.DeletionProtection ?? false}
        onDeleted={async () =>
          await navigate({ to: "/rds/describe-db-instances" })
        }
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
