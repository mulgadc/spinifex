import type { Volume } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Link2, Unlink } from "lucide-react"
import { useState } from "react"

import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"

import { AttachVolumeForm } from "./attach-volume-form"
import { DetachVolumeDialog } from "./detach-volume-dialog"

// An available volume with no attachment can be attached, and nothing else.
export function isVolumeAvailable(volume: Volume): boolean {
  return volume.State === "available" && !volume.Attachments?.length
}

// The volume's attachment, with attach and detach as its own page offers them.
export function VolumeAttachment({ volume }: { volume: Volume }) {
  const volumeId = volume.VolumeId ?? ""
  const attachments = volume.Attachments ?? []
  const [dialog, setDialog] = useState<"attach" | "detach" | null>(null)

  function onOpenChange(open: boolean) {
    if (!open) {
      setDialog(null)
    }
  }

  return (
    <PanelSection title="Attachment">
      <div className="flex flex-col gap-3">
        {attachments.length === 0 ? (
          <p className="text-sm text-muted-foreground">Not attached.</p>
        ) : (
          attachments.map((attachment) => (
            <PanelFields key={attachment.InstanceId}>
              <PanelField label="Instance" mono>
                {attachment.InstanceId && (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: attachment.InstanceId }}
                    to="/ec2/describe-instances/$id"
                  >
                    {attachment.InstanceId}
                  </Link>
                )}
              </PanelField>
              <PanelField label="Device" mono>
                {attachment.Device}
              </PanelField>
              <PanelField label="Status">{attachment.State}</PanelField>
              <PanelField label="Attached">
                {formatDateTime(attachment.AttachTime)}
              </PanelField>
              <PanelField label="Delete on termination">
                {attachment.DeleteOnTermination ? "Yes" : "No"}
              </PanelField>
            </PanelFields>
          ))
        )}
        <div className="flex flex-wrap gap-2">
          <Button
            disabled={!isVolumeAvailable(volume)}
            onClick={() => {
              setDialog("attach")
            }}
            size="sm"
            variant="outline"
          >
            <Link2 data-icon="inline-start" />
            Attach
          </Button>
          <Button
            disabled={attachments.length === 0}
            onClick={() => {
              setDialog("detach")
            }}
            size="sm"
            variant="outline"
          >
            <Unlink data-icon="inline-start" />
            Detach
          </Button>
        </div>
      </div>
      {dialog === "attach" && (
        <AttachVolumeForm
          onClose={() => {
            setDialog(null)
          }}
          volumeId={volumeId}
          zone={volume.AvailabilityZone}
        />
      )}
      <DetachVolumeDialog
        instanceId={attachments[0]?.InstanceId}
        onOpenChange={onOpenChange}
        open={dialog === "detach"}
        volumeId={volumeId}
      />
    </PanelSection>
  )
}
