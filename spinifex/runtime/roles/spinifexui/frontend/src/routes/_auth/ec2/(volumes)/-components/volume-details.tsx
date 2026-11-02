import type { Volume } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import {
  Camera,
  Eye,
  Maximize2,
  Scaling,
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
import { useDeleteVolume } from "@/mutations/ec2"

import { ModifyVolumeForm } from "./modify-volume-form"
import { isVolumeAvailable, VolumeAttachment } from "./volume-attachment"

function Overview({ volume }: { volume: Volume }) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={volume.VolumeId} label="Volume ID" mono>
            {volume.VolumeId}
          </PanelField>
          <PanelField label="Size">
            {volume.Size === undefined ? undefined : `${volume.Size} GiB`}
          </PanelField>
          <PanelField label="Volume type">{volume.VolumeType}</PanelField>
          {volume.Iops !== undefined && (
            <PanelField label="IOPS">{volume.Iops.toString()}</PanelField>
          )}
          <PanelField label="Availability zone">
            {volume.AvailabilityZone}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(volume.CreateTime)}
          </PanelField>
          <PanelField label="Snapshot" mono>
            {volume.SnapshotId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: volume.SnapshotId }}
                to="/ec2/describe-snapshots/$id"
              >
                {volume.SnapshotId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Encrypted">
            {volume.Encrypted ? "Yes" : "No"}
          </PanelField>
          <PanelField label="KMS key" mono>
            {volume.KmsKeyId}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <VolumeAttachment volume={volume} />
    </>
  )
}

function Settings({
  volume,
  onDeleted,
}: {
  volume: Volume
  onDeleted: () => void
}) {
  const id = volume.VolumeId ?? ""
  const available = isVolumeAvailable(volume)
  const remove = useDeleteVolume()
  const [confirm, setConfirm] = useState(false)
  const [resizing, setResizing] = useState(false)

  return (
    <>
      <Ec2Tags
        id={volume.VolumeId}
        invalidate={["ec2", "volumes"]}
        tags={volume.Tags}
      />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {!available && (
            <p className="text-sm text-muted-foreground">
              Detach the volume before resizing or deleting it.
            </p>
          )}
          <div className="flex flex-wrap gap-2">
            <Button
              disabled={!available}
              onClick={() => {
                setResizing(true)
              }}
              size="sm"
              variant="outline"
            >
              <Scaling data-icon="inline-start" />
              Resize
            </Button>
            <Link
              className={buttonVariants({ size: "sm", variant: "outline" })}
              search={{ create: true, volume: id }}
              to="/ec2/describe-snapshots"
            >
              <Camera data-icon="inline-start" />
              Create snapshot
            </Link>
          </div>
          <Button
            disabled={!available || remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete volume
          </Button>
        </div>
      </PanelSection>
      {resizing && (
        <ModifyVolumeForm
          currentSize={volume.Size ?? 1}
          onClose={() => {
            setResizing(false)
          }}
          volumeId={id}
        />
      )}
      <ResourceDeleteDialog
        confirmPhrase="delete"
        description={`Delete ${id}? Its data is lost unless it has a snapshot. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete volume"
      />
    </>
  )
}

interface VolumeDetailsProps {
  volume: Volume
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One volume's details, as the list's side panel or as its own page.
export function VolumeDetails({
  volume,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: VolumeDetailsProps) {
  const id = volume.VolumeId ?? ""
  const name = getNameTag(volume.Tags)

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to volumes" : "Close volume details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open volume page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-volumes/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Volume details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={<StateBadge state={volume.State} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview volume={volume} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} volume={volume} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
