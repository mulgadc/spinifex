import type { DBSnapshot } from "@aws-sdk/client-rds"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2, Undo2 } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { rdsSnapshotEventsQueryOptions } from "@/queries/rds"
import {
  canDeleteSnapshot,
  canRestoreSnapshot,
  SNAPSHOT_TYPE_AUTOMATED,
} from "@/types/rds"

import { RdsEventsPanel } from "../../-components/rds-events-panel"
import { RdsTagsTab } from "../../-components/rds-tags-tab"
import { DeleteDBSnapshotDialog } from "./delete-db-snapshot-dialog"

function Overview({ snapshot }: { snapshot: DBSnapshot }) {
  const sourceId = snapshot.DBInstanceIdentifier ?? ""

  return (
    <>
      <PanelSection title="Snapshot">
        <PanelFields>
          <PanelField label="Type">{snapshot.SnapshotType}</PanelField>
          <PanelField label="Status">{snapshot.Status}</PanelField>
          <PanelField label="Created">
            {formatDateTime(snapshot.SnapshotCreateTime)}
          </PanelField>
          <PanelField label="Progress">
            {snapshot.PercentProgress === undefined
              ? undefined
              : `${snapshot.PercentProgress}%`}
          </PanelField>
          <PanelField copy={snapshot.DBSnapshotArn} label="ARN" mono>
            {snapshot.DBSnapshotArn}
          </PanelField>
        </PanelFields>
      </PanelSection>

      <PanelSection title="Source">
        <PanelFields>
          <PanelField label="DB instance" mono>
            {sourceId !== "" && (
              <Link
                className="text-primary hover:underline"
                params={{ id: sourceId }}
                to="/rds/describe-db-instances/$id"
              >
                {sourceId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Engine">{snapshot.Engine}</PanelField>
          <PanelField label="Version">{snapshot.EngineVersion}</PanelField>
          <PanelField label="Allocated storage">
            {snapshot.AllocatedStorage
              ? `${snapshot.AllocatedStorage} GiB`
              : undefined}
          </PanelField>
          <PanelField label="Storage type">{snapshot.StorageType}</PanelField>
          <PanelField label="Encryption">
            {snapshot.Encrypted ? "Encrypted — always on" : "Not encrypted"}
          </PanelField>
          <PanelField label="Master username">
            {snapshot.MasterUsername}
          </PanelField>
          <PanelField label="Port" mono>
            {snapshot.Port?.toString()}
          </PanelField>
          <PanelField label="VPC" mono>
            {snapshot.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: snapshot.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {snapshot.VpcId}
              </Link>
            )}
          </PanelField>
        </PanelFields>
        <p className="mt-2 text-xs text-muted-foreground">
          A restore starts on this snapshot&apos;s datadir, so the engine, the
          master credentials and the initial database come with it and cannot be
          changed.
        </p>
      </PanelSection>
    </>
  )
}

function Settings({ snapshot }: { snapshot: DBSnapshot }) {
  const { data } = useQuery(
    rdsSnapshotEventsQueryOptions(snapshot.DBSnapshotIdentifier ?? ""),
  )

  return (
    <>
      <PanelSection title="Tags">
        <RdsTagsTab arn={snapshot.DBSnapshotArn ?? ""} />
      </PanelSection>
      <PanelSection title="Events">
        <RdsEventsPanel events={data?.Events ?? []} />
      </PanelSection>
    </>
  )
}

// Restore opens its own form; delete confirms here. Both stay visible and
// disable by status, so an automated backup shows why it cannot be deleted.
function SnapshotActions({
  snapshot,
  onDeleted,
}: {
  snapshot: DBSnapshot
  onDeleted: () => void
}) {
  const id = snapshot.DBSnapshotIdentifier ?? ""
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <div className="flex flex-wrap items-center justify-center gap-2">
        {canRestoreSnapshot(snapshot.Status) ? (
          <Link
            className={buttonVariants({ size: "sm", variant: "outline" })}
            params={{ id }}
            to="/rds/restore-db-instance-from-db-snapshot/$id"
          >
            <Undo2 data-icon="inline-start" />
            Restore
          </Link>
        ) : (
          <Button disabled size="sm" variant="outline">
            <Undo2 data-icon="inline-start" />
            Restore
          </Button>
        )}
        <Button
          disabled={!canDeleteSnapshot(snapshot.Status, snapshot.SnapshotType)}
          onClick={() => {
            setConfirm(true)
          }}
          size="sm"
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete
        </Button>
      </div>
      {snapshot.SnapshotType === SNAPSHOT_TYPE_AUTOMATED && (
        <p className="text-center text-xs text-muted-foreground">
          An automated backup: it is deleted by the source instance&apos;s
          backup retention, not by hand.
        </p>
      )}
      <DeleteDBSnapshotDialog
        dbSnapshotIdentifier={id}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
      />
    </>
  )
}

interface DBSnapshotDetailsProps {
  snapshot: DBSnapshot
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One DB snapshot's details, as the list's side panel or as its own page.
export function DBSnapshotDetails({
  snapshot,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: DBSnapshotDetailsProps) {
  const id = snapshot.DBSnapshotIdentifier ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to snapshots" : "Close snapshot details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open snapshot page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/rds/describe-db-snapshots/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Snapshot details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={snapshot.DBInstanceIdentifier}
      summary={
        <>
          <StateBadge state={snapshot.Status} />
          <SnapshotActions onDeleted={onClose} snapshot={snapshot} />
        </>
      }
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview snapshot={snapshot} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings snapshot={snapshot} />,
        },
      ]}
      title={id}
    />
  )
}
