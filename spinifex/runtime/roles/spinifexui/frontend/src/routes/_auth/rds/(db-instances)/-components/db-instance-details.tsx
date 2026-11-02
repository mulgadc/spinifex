import type { DBInstance, Event } from "@aws-sdk/client-rds"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal } from "lucide-react"

import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import {
  rdsAutomatedBackupsQueryOptions,
  rdsEventsQueryOptions,
  rdsInstanceDBSnapshotsQueryOptions,
} from "@/queries/rds"
import { SNAPSHOT_TYPE_AUTOMATED } from "@/types/rds"

import { DBSnapshotList } from "../../(snapshots)/-components/db-snapshot-list"
import { RdsEventsPanel } from "../../-components/rds-events-panel"
import { RdsTagsTab } from "../../-components/rds-tags-tab"
import { DBInstanceActions } from "./db-instance-actions"

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

function gib(size: number | undefined): string | undefined {
  return size ? `${size} GiB` : undefined
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

function Overview({ instance }: { instance: DBInstance }) {
  const address = instance.Endpoint?.Address
  const port = (instance.Endpoint?.Port ?? instance.DbInstancePort)?.toString()
  const vpcId = instance.DBSubnetGroup?.VpcId
  const subnetGroup = instance.DBSubnetGroup?.DBSubnetGroupName
  const securityGroups = instance.VpcSecurityGroups ?? []
  const pending = instance.PendingModifiedValues

  const hasPending =
    pending?.AllocatedStorage !== undefined ||
    pending?.DBInstanceClass !== undefined

  return (
    <>
      <PanelSection title="Connectivity">
        <PanelFields>
          <PanelField copy={address} label="Address" mono>
            {address}
          </PanelField>
          <PanelField label="Port" mono>
            {port}
          </PanelField>
          <PanelField label="VPC" mono>
            {vpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: vpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {vpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="DB subnet group" mono>
            {subnetGroup && (
              <Link
                className="text-primary hover:underline"
                params={{ name: subnetGroup }}
                to="/rds/describe-db-subnet-groups/$name"
              >
                {subnetGroup}
              </Link>
            )}
          </PanelField>
          <PanelField label="Security groups" mono>
            {securityGroups.length > 0 && (
              <span className="flex flex-col items-end">
                {securityGroups.map((group) => (
                  <Link
                    className="text-primary hover:underline"
                    key={group.VpcSecurityGroupId}
                    params={{ id: group.VpcSecurityGroupId ?? "" }}
                    to="/ec2/describe-security-groups/$id"
                  >
                    {group.VpcSecurityGroupId}
                  </Link>
                ))}
              </span>
            )}
          </PanelField>
          <PanelField label="Publicly accessible">
            No — private VPC address
          </PanelField>
        </PanelFields>
      </PanelSection>

      <CliCommandPanel commands={buildConnectCommand(instance)} />

      <PanelSection title="Configuration">
        <PanelFields>
          <PanelField label="Engine">{instance.Engine}</PanelField>
          <PanelField label="Version">{instance.EngineVersion}</PanelField>
          <PanelField label="Instance class">
            {instance.DBInstanceClass}
          </PanelField>
          <PanelField label="Allocated storage">
            {gib(instance.AllocatedStorage)}
          </PanelField>
          <PanelField label="Storage type">{instance.StorageType}</PanelField>
          <PanelField label="Encryption">
            {instance.StorageEncrypted
              ? "Encrypted — always on"
              : "Not encrypted"}
          </PanelField>
          <PanelField label="Parameter group">
            {instance.DBParameterGroups?.map(
              (g) => `${g.DBParameterGroupName} (${g.ParameterApplyStatus})`,
            ).join(", ")}
          </PanelField>
          <PanelField label="Master username">
            {instance.MasterUsername}
          </PanelField>
          <PanelField label="Initial database">{instance.DBName}</PanelField>
          <PanelField label="Deletion protection">
            {instance.DeletionProtection ? "On" : "Off"}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(instance.InstanceCreateTime)}
          </PanelField>
          <PanelField copy={instance.DBInstanceArn} label="ARN" mono>
            {instance.DBInstanceArn}
          </PanelField>
        </PanelFields>
      </PanelSection>

      {hasPending && (
        <PanelSection title="Pending changes">
          <PanelFields>
            <PanelField label="Instance class">
              {pending?.DBInstanceClass}
            </PanelField>
            <PanelField label="Allocated storage">
              {gib(pending?.AllocatedStorage)}
            </PanelField>
          </PanelFields>
        </PanelSection>
      )}
    </>
  )
}

function BackupAlert({ title, event }: { title: string; event: Event }) {
  return (
    <div
      className="rounded-md border border-warning-border bg-warning-subtle p-3 text-sm"
      role="alert"
    >
      <p className="font-medium">{title}</p>
      <p className="mt-1 text-xs text-muted-foreground">
        {event.Message} ({formatDateTime(event.Date)})
      </p>
    </div>
  )
}

function Settings({ instance }: { instance: DBInstance }) {
  const id = instance.DBInstanceIdentifier ?? ""
  const { data: eventsData } = useQuery(rdsEventsQueryOptions(id))

  const { data: snapshotsData } = useQuery(
    rdsInstanceDBSnapshotsQueryOptions(id),
  )

  const { data: automatedBackupsData } = useQuery(
    rdsAutomatedBackupsQueryOptions(id),
  )

  const events = eventsData?.Events ?? []
  const snapshots = snapshotsData?.DBSnapshots ?? []
  const automatedBackup = automatedBackupsData?.DBInstanceAutomatedBackups?.[0]

  // The record's backup failure counters are internal and never projected onto
  // a DB instance, so the API-visible signal is the failure event itself.
  const lastBackupFailure = latestEvent(
    events,
    (event) => hasCategory(event, "backup") && hasCategory(event, "failure"),
  )

  // A quiesce that could not be taken or released is recorded as a backup
  // notification: the only signal that a snapshot is crash consistent.
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
    <>
      {lastBackupFailure && (
        <BackupAlert
          event={lastBackupFailure}
          title="An automated backup failed"
        />
      )}
      {lastBackupWarning && (
        <BackupAlert
          event={lastBackupWarning}
          title="A backup raised a warning"
        />
      )}

      <PanelSection title="Automated backups">
        <PanelFields>
          <PanelField label="Retention">
            {instance.BackupRetentionPeriod === 0
              ? "Disabled"
              : `${instance.BackupRetentionPeriod} days`}
          </PanelField>
          <PanelField label="Status">
            {automatedBackup?.Status ?? "None — automated backups are off"}
          </PanelField>
          <PanelField label="Backup window">
            {instance.PreferredBackupWindow}
          </PanelField>
          <PanelField label="Maintenance window">
            {instance.PreferredMaintenanceWindow}
          </PanelField>
          <PanelField label="Last backup">
            {lastBackup?.SnapshotCreateTime &&
              formatDateTime(lastBackup.SnapshotCreateTime)}
          </PanelField>
        </PanelFields>
        <p className="mt-2 text-xs text-muted-foreground">
          Automated backups are daily snapshots taken in the backup window,
          deleted by retention rather than by hand. There is no point-in-time
          restore.
        </p>
      </PanelSection>

      <PanelSection title={`Snapshots (${snapshots.length})`}>
        <DBSnapshotList snapshots={snapshots} />
      </PanelSection>

      <PanelSection title="Tags">
        <RdsTagsTab arn={instance.DBInstanceArn ?? ""} />
      </PanelSection>

      <PanelSection title="Events">
        <RdsEventsPanel events={events} />
      </PanelSection>
    </>
  )
}

interface DBInstanceDetailsProps {
  instance: DBInstance
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One DB instance's details, as the list's side panel or as its own page.
export function DBInstanceDetails({
  instance,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: DBInstanceDetailsProps) {
  const id = instance.DBInstanceIdentifier ?? ""

  const engine = [instance.Engine, instance.EngineVersion]
    .filter(Boolean)
    .join(" ")

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to databases" : "Close database details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open database page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/rds/describe-db-instances/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Database details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={engine || undefined}
      summary={
        <>
          <div className="flex flex-wrap items-center justify-center gap-2">
            <StateBadge state={instance.DBInstanceStatus} />
            {instance.StatusInfos?.map((info) => (
              <span
                className="text-xs text-warning-foreground"
                key={`${info.StatusType}-${info.Status}`}
              >
                {info.StatusType}: {info.Message ?? info.Status}
              </span>
            ))}
          </div>
          <DBInstanceActions
            instance={instance}
            layout={layout}
            onDeleted={onClose}
            tab={tab}
          />
        </>
      }
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview instance={instance} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings instance={instance} />,
        },
      ]}
      title={id}
    />
  )
}
