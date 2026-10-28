import type {
  DBInstance,
  DBInstanceAutomatedBackup,
  DBSnapshot,
  Event,
} from "@aws-sdk/client-rds"

import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { formatDateTime } from "@/lib/utils"
import { SNAPSHOT_TYPE_AUTOMATED } from "@/types/rds"

import { DBSnapshotsTable } from "../../(snapshots)/-components/db-snapshots-table"

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

function formatGiB(value: number | undefined): string | undefined {
  return value ? `${value} GiB` : undefined
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

export function ConnectivityTab({ instance }: { instance: DBInstance }) {
  return (
    <div className="space-y-4">
      <DetailCard>
        <DetailCard.Header>Endpoint</DetailCard.Header>
        <DetailCard.Content>
          <DetailRow label="Address" value={instance.Endpoint?.Address} />
          <DetailRow
            label="Port"
            value={(
              instance.Endpoint?.Port ?? instance.DbInstancePort
            )?.toString()}
          />
          <DetailRow label="VPC" value={instance.DBSubnetGroup?.VpcId} />
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
  )
}

function PendingChangesCard({ instance }: { instance: DBInstance }) {
  const pending = instance.PendingModifiedValues

  const hasPending =
    pending?.AllocatedStorage !== undefined ||
    pending?.DBInstanceClass !== undefined

  if (!hasPending) {
    return null
  }

  return (
    <DetailCard>
      <DetailCard.Header>Pending Changes</DetailCard.Header>
      <DetailCard.Content>
        <DetailRow label="Instance class" value={pending.DBInstanceClass} />
        <DetailRow
          label="Allocated storage"
          value={formatGiB(pending.AllocatedStorage)}
        />
      </DetailCard.Content>
    </DetailCard>
  )
}

export function ConfigurationTab({ instance }: { instance: DBInstance }) {
  return (
    <div className="space-y-4">
      <DetailCard>
        <DetailCard.Header>Configuration</DetailCard.Header>
        <DetailCard.Content>
          <DetailRow label="Engine" value={instance.Engine} />
          <DetailRow label="Version" value={instance.EngineVersion} />
          <DetailRow label="Instance class" value={instance.DBInstanceClass} />
          <DetailRow
            label="Allocated storage"
            value={formatGiB(instance.AllocatedStorage)}
          />
          <DetailRow label="Storage type" value={instance.StorageType} />
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
              (g) => `${g.DBParameterGroupName} (${g.ParameterApplyStatus})`,
            ).join(", ")}
          />
          <DetailRow label="Master username" value={instance.MasterUsername} />
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

      <PendingChangesCard instance={instance} />
    </div>
  )
}

function BackupEventAlert({
  event,
  title,
}: {
  event: Event | undefined
  title: string
}) {
  if (!event) {
    return null
  }

  return (
    <div
      className="rounded-md border border-tactical-amber/40 bg-tactical-amber/5 p-4 text-sm"
      role="alert"
    >
      <p className="font-medium">{title}</p>
      <p className="mt-1 text-xs text-muted-foreground">
        {event.Message} ({formatDateTime(event.Date)})
      </p>
    </div>
  )
}

interface BackupsTabProps {
  automatedBackup: DBInstanceAutomatedBackup | undefined
  events: Event[]
  instance: DBInstance
  onDeleteSnapshot: (dbSnapshotIdentifier: string) => void
  snapshots: DBSnapshot[]
}

export function BackupsTab({
  automatedBackup,
  events,
  instance,
  onDeleteSnapshot,
  snapshots,
}: BackupsTabProps) {
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

  return (
    <div className="space-y-4">
      <BackupEventAlert
        event={lastBackupFailure}
        title="An automated backup failed"
      />
      <BackupEventAlert
        event={lastBackupWarning}
        title="A backup raised a warning"
      />

      <DetailCard>
        <DetailCard.Header>Automated Backups</DetailCard.Header>
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
              automatedBackup?.Status ?? "None — automated backups are off"
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
          <DBSnapshotsTable
            onDelete={onDeleteSnapshot}
            showSourceColumns={false}
            snapshots={snapshots}
          />
        ) : (
          <p className="text-muted-foreground">
            No snapshots of this instance.
          </p>
        )}
      </div>

      <p className="text-xs text-muted-foreground">
        Automated backups are daily snapshots taken in the backup window,
        deleted by retention rather than by hand. There is no point-in-time
        restore.
      </p>
    </div>
  )
}
