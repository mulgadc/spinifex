import type { Snapshot } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import {
  Copy,
  Eye,
  Image,
  Maximize2,
  SlidersHorizontal,
  Trash2,
} from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Ec2Tags } from "@/components/resource-tags"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime, getNameTag } from "@/lib/utils"
import { useDeleteSnapshot } from "@/mutations/ec2"

import { CopySnapshotForm } from "./copy-snapshot-form"
import { RegisterImageForm } from "./register-image-form"

function Overview({ snapshot }: { snapshot: Snapshot }) {
  return (
    <PanelSection title="Details">
      <PanelFields>
        <PanelField copy={snapshot.SnapshotId} label="Snapshot ID" mono>
          {snapshot.SnapshotId}
        </PanelField>
        <PanelField label="Volume" mono>
          {snapshot.VolumeId && (
            <Link
              className="text-primary hover:underline"
              params={{ id: snapshot.VolumeId }}
              to="/ec2/describe-volumes/$id"
            >
              {snapshot.VolumeId}
            </Link>
          )}
        </PanelField>
        <PanelField label="Volume size">
          {snapshot.VolumeSize === undefined
            ? undefined
            : `${snapshot.VolumeSize} GiB`}
        </PanelField>
        <PanelField label="Description">
          {snapshot.Description && (
            <span title={snapshot.Description}>{snapshot.Description}</span>
          )}
        </PanelField>
        <PanelField label="Progress">{snapshot.Progress}</PanelField>
        <PanelField label="Started">
          {formatDateTime(snapshot.StartTime)}
        </PanelField>
        <PanelField label="Encrypted">
          {snapshot.Encrypted ? "Yes" : "No"}
        </PanelField>
        <PanelField label="Owner" mono>
          {snapshot.OwnerId}
        </PanelField>
      </PanelFields>
    </PanelSection>
  )
}

function Settings({
  snapshot,
  onDeleted,
}: {
  snapshot: Snapshot
  onDeleted: () => void
}) {
  const id = snapshot.SnapshotId ?? ""
  const completed = snapshot.State === "completed"
  const remove = useDeleteSnapshot()

  const [dialog, setDialog] = useState<"copy" | "register" | "delete" | null>(
    null,
  )

  function onOpenChange(open: boolean) {
    if (!open) {
      setDialog(null)
    }
  }

  return (
    <>
      <Ec2Tags
        id={snapshot.SnapshotId}
        invalidate={["ec2", "snapshots"]}
        tags={snapshot.Tags}
      />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {!completed && (
            <p className="text-sm text-muted-foreground">
              A snapshot can be copied or registered once it has completed.
            </p>
          )}
          <div className="flex flex-wrap gap-2">
            <Button
              disabled={!completed}
              onClick={() => {
                setDialog("copy")
              }}
              size="sm"
              variant="outline"
            >
              <Copy data-icon="inline-start" />
              Copy snapshot
            </Button>
            <Button
              disabled={!completed}
              onClick={() => {
                setDialog("register")
              }}
              size="sm"
              variant="outline"
            >
              <Image data-icon="inline-start" />
              Register as AMI
            </Button>
          </div>
          <Button
            disabled={remove.isPending}
            onClick={() => {
              setDialog("delete")
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete snapshot
          </Button>
        </div>
      </PanelSection>
      {dialog === "copy" && (
        <CopySnapshotForm
          onClose={() => {
            setDialog(null)
          }}
          snapshotId={id}
        />
      )}
      {dialog === "register" && (
        <RegisterImageForm
          onClose={() => {
            setDialog(null)
          }}
          snapshotId={id}
        />
      )}
      <ResourceDeleteDialog
        confirmPhrase="delete"
        description={`Delete ${id}? An AMI registered from it must be deregistered first. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={onOpenChange}
        open={dialog === "delete"}
        title="Delete snapshot"
      />
    </>
  )
}

interface SnapshotDetailsProps {
  snapshot: Snapshot
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One snapshot's details, as the list's side panel or as its own page.
export function SnapshotDetails({
  snapshot,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: SnapshotDetailsProps) {
  const id = snapshot.SnapshotId ?? ""
  const name = getNameTag(snapshot.Tags)

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
            to="/ec2/describe-snapshots/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Snapshot details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={<StateBadge state={snapshot.State} />}
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
          content: <Settings onDeleted={onClose} snapshot={snapshot} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
