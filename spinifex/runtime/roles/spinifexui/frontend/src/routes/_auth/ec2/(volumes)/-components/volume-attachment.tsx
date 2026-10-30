import type { Instance, Volume } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Link2, Unlink } from "lucide-react"
import { useState } from "react"

import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { formatDateTime, getNameTag } from "@/lib/utils"
import { useAttachVolume, useDetachVolume } from "@/mutations/ec2"
import { ec2InstancesQueryOptions } from "@/queries/ec2"

// An available volume with no attachment can be attached, and nothing else.
export function isVolumeAvailable(volume: Volume): boolean {
  return volume.State === "available" && !volume.Attachments?.length
}

// AWS attaches only to a running or stopped instance in the volume's zone.
function isAttachTarget(instance: Instance, zone: string | undefined) {
  const state = instance.State?.Name
  const inZone = !zone || instance.Placement?.AvailabilityZone === zone

  return (state === "running" || state === "stopped") && inZone
}

function AttachDialog({
  volumeId,
  zone,
  open,
  onOpenChange,
}: {
  volumeId: string
  zone: string | undefined
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { data } = useQuery(ec2InstancesQueryOptions)
  const attach = useAttachVolume()
  const [instanceId, setInstanceId] = useState("")
  const [device, setDevice] = useState("")

  const targets =
    data?.Reservations?.flatMap(
      (r) => r.Instances?.filter((i) => isAttachTarget(i, zone)) ?? [],
    ) ?? []

  const where = zone ? ` in ${zone}` : ""

  function onChange(next: boolean) {
    if (!next) {
      setInstanceId("")
      setDevice("")
      attach.reset()
    }

    onOpenChange(next)
  }

  async function onAttach() {
    try {
      await attach.mutateAsync({
        volumeId,
        instanceId,
        device: device || undefined,
      })
      onChange(false)
    } catch {
      // Shown in the dialog.
    }
  }

  return (
    <AlertDialog onOpenChange={onChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Attach volume</AlertDialogTitle>
          <AlertDialogDescription>
            Attach {volumeId} to a running or stopped instance{where}.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {attach.error && (
          <ErrorBanner error={attach.error} msg="Failed to attach volume" />
        )}
        <Field>
          <FieldTitle>
            <label htmlFor="attachInstance">Instance</label>
          </FieldTitle>
          <Select
            onValueChange={(value) => {
              setInstanceId(value ?? "")
            }}
            value={instanceId}
          >
            <SelectTrigger className="w-full" id="attachInstance">
              <SelectValue
                placeholder={
                  targets.length
                    ? "Select an instance"
                    : `No running or stopped instances${where}.`
                }
              />
            </SelectTrigger>
            <SelectContent>
              {targets.map((instance) => {
                const name = getNameTag(instance.Tags)

                return (
                  <SelectItem
                    key={instance.InstanceId}
                    value={instance.InstanceId ?? ""}
                  >
                    {instance.InstanceId}
                    {name ? ` (${name})` : ""}
                  </SelectItem>
                )
              })}
            </SelectContent>
          </Select>
        </Field>
        <Field>
          <FieldTitle>
            <label htmlFor="attachDevice">Device (optional)</label>
          </FieldTitle>
          <Input
            id="attachDevice"
            onChange={(e) => {
              setDevice(e.target.value)
            }}
            placeholder="/dev/sdf"
            value={device}
          />
        </Field>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={attach.isPending || !instanceId}
            onClick={() => {
              void onAttach()
            }}
          >
            {attach.isPending ? "Attaching…" : "Attach"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

function DetachDialog({
  volumeId,
  instanceId,
  open,
  onOpenChange,
}: {
  volumeId: string
  instanceId: string | undefined
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const detach = useDetachVolume()
  const [force, setForce] = useState(false)

  function onChange(next: boolean) {
    if (!next) {
      setForce(false)
      detach.reset()
    }

    onOpenChange(next)
  }

  async function onDetach() {
    try {
      await detach.mutateAsync({
        volumeId,
        instanceId,
        force: force || undefined,
      })
      onChange(false)
    } catch {
      // Shown in the dialog.
    }
  }

  return (
    <AlertDialog onOpenChange={onChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Detach volume</AlertDialogTitle>
          <AlertDialogDescription>
            Detach {volumeId} from {instanceId}? Unmount it inside the instance
            first, or data still being written may be lost.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {detach.error && (
          <ErrorBanner error={detach.error} msg="Failed to detach volume" />
        )}
        <div className="flex items-center gap-2">
          <Switch checked={force} id="forceDetach" onCheckedChange={setForce} />
          <Label htmlFor="forceDetach">Force detach</Label>
        </div>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={detach.isPending}
            onClick={() => {
              void onDetach()
            }}
          >
            {detach.isPending ? "Detaching…" : "Detach"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
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
      <AttachDialog
        onOpenChange={onOpenChange}
        open={dialog === "attach"}
        volumeId={volumeId}
        zone={volume.AvailabilityZone}
      />
      <DetachDialog
        instanceId={attachments[0]?.InstanceId}
        onOpenChange={onOpenChange}
        open={dialog === "detach"}
        volumeId={volumeId}
      />
    </PanelSection>
  )
}
