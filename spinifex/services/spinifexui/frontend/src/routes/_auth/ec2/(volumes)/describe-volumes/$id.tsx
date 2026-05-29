import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { Camera, Link2, Link2Off, Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
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
import { Button, buttonVariants } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { formatDateTime, getNameTag } from "@/lib/utils"
import {
  useAttachVolume,
  useDeleteVolume,
  useDetachVolume,
} from "@/mutations/ec2"
import { ec2InstancesQueryOptions, ec2VolumeQueryOptions } from "@/queries/ec2"

type DialogType = "delete" | "attach" | "detach" | null

export const Route = createFileRoute(
  "/_auth/ec2/(volumes)/describe-volumes/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2VolumeQueryOptions(params.id)),
      context.queryClient.ensureQueryData(ec2InstancesQueryOptions),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | EC2 | Mulga`,
      },
    ],
  }),
  component: VolumeDetail,
})

function VolumeDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2VolumeQueryOptions(id))
  const { data: instancesData } = useSuspenseQuery(ec2InstancesQueryOptions)
  const volume = data.Volumes?.[0]
  const deleteMutation = useDeleteVolume()
  const attachMutation = useAttachVolume()
  const detachMutation = useDetachVolume()
  const [activeDialog, setActiveDialog] = useState<DialogType>(null)
  const [attachInstanceId, setAttachInstanceId] = useState("")
  const [attachDevice, setAttachDevice] = useState("")
  const [detachForce, setDetachForce] = useState(false)

  const runningInstances =
    instancesData.Reservations?.flatMap(
      (r) => r.Instances?.filter((i) => i.State?.Name === "running") ?? [],
    ) ?? []

  const isAvailable =
    volume?.State === "available" &&
    (!volume.Attachments || volume.Attachments.length === 0)

  const closeDialog = () => setActiveDialog(null)

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(id)
      navigate({ to: "/ec2/describe-volumes" })
    } finally {
      closeDialog()
    }
  }

  const handleAttach = async () => {
    try {
      await attachMutation.mutateAsync({
        volumeId: id,
        instanceId: attachInstanceId,
        device: attachDevice || undefined,
      })
      closeDialog()
      setAttachInstanceId("")
      setAttachDevice("")
    } catch {
      // error shown via attachMutation.error
    }
  }

  const handleDetach = async () => {
    const instanceId = volume?.Attachments?.[0]?.InstanceId
    try {
      await detachMutation.mutateAsync({
        volumeId: id,
        instanceId: instanceId ?? undefined,
        force: detachForce || undefined,
      })
      closeDialog()
      setDetachForce(false)
    } catch {
      // error shown via detachMutation.error
    }
  }

  if (!volume?.VolumeId) {
    return (
      <>
        <BackLink to="/ec2/describe-volumes">Back to volumes</BackLink>
        <p className="text-muted-foreground">Volume not found.</p>
      </>
    )
  }

  return (
    <>
      <BackLink to="/ec2/describe-volumes">Back to volumes</BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete volume"
        />
      )}

      {attachMutation.error && (
        <ErrorBanner
          error={attachMutation.error}
          msg="Failed to attach volume"
        />
      )}

      {detachMutation.error && (
        <ErrorBanner
          error={detachMutation.error}
          msg="Failed to detach volume"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                disabled={!isAvailable}
                onClick={() => setActiveDialog("delete")}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
              {isAvailable ? (
                <Link
                  className={buttonVariants({ variant: "outline" })}
                  params={{ id: volume.VolumeId }}
                  to="/ec2/modify-volume/$id"
                >
                  Resize Volume
                </Link>
              ) : (
                <Button disabled variant="outline">
                  Resize Volume
                </Button>
              )}
              {isAvailable ? (
                <Button
                  onClick={() => setActiveDialog("attach")}
                  size="sm"
                  variant="outline"
                >
                  <Link2 className="size-4" />
                  Attach
                </Button>
              ) : null}
              {volume.Attachments && volume.Attachments.length > 0 ? (
                <Button
                  onClick={() => setActiveDialog("detach")}
                  size="sm"
                  variant="outline"
                >
                  <Link2Off className="size-4" />
                  Detach
                </Button>
              ) : null}
              <Link
                className={buttonVariants({ variant: "outline", size: "sm" })}
                search={{ volumeId: volume.VolumeId }}
                to="/ec2/create-snapshot"
              >
                <Camera className="size-4" />
                Create Snapshot
              </Link>
              <StateBadge state={volume.State} />
            </div>
          }
          subtitle="Volume Details"
          title={volume.VolumeId}
        />

        <DetailCard>
          <DetailCard.Header>Volume Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Volume ID" value={volume.VolumeId} />
            <DetailRow label="Size" value={`${volume.Size} GiB`} />
            <DetailRow label="Volume Type" value={volume.VolumeType} />
            <DetailRow label="State" value={volume.State} />
            <DetailRow
              label="Availability Zone"
              value={volume.AvailabilityZone}
            />
            <DetailRow
              label="Create Time"
              value={formatDateTime(volume.CreateTime)}
            />
            <DetailRow
              label="Encrypted"
              value={volume.Encrypted ? "Yes" : "No"}
            />
            <DetailRow label="KMS Key ID" value={volume.KmsKeyId} />
          </DetailCard.Content>
        </DetailCard>

        {volume.Attachments && volume.Attachments.length > 0 && (
          <DetailCard>
            <DetailCard.Header>Attachments</DetailCard.Header>
            {volume.Attachments.map((attachment) => (
              <DetailCard.Content key={attachment.InstanceId}>
                <DetailRow
                  label="Instance ID"
                  value={
                    attachment.InstanceId ? (
                      <Link
                        className="text-primary hover:underline"
                        params={{ id: attachment.InstanceId }}
                        to="/ec2/describe-instances/$id"
                      >
                        {attachment.InstanceId}
                      </Link>
                    ) : undefined
                  }
                />
                <DetailRow label="Device" value={attachment.Device} />
                <DetailRow label="Status" value={attachment.State} />
                <DetailRow
                  label="Attach Time"
                  value={formatDateTime(attachment.AttachTime)}
                />
                <DetailRow
                  label="Delete on Termination"
                  value={attachment.DeleteOnTermination ? "Yes" : "No"}
                />
              </DetailCard.Content>
            ))}
          </DetailCard>
        )}
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the volume "${volume.VolumeId}"? This action cannot be undone.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={(open) => {
          if (!open) {
            closeDialog()
          }
        }}
        open={activeDialog === "delete"}
        title="Delete Volume"
      />

      <AlertDialog
        onOpenChange={(open) => {
          if (!open) {
            closeDialog()
            setAttachInstanceId("")
            setAttachDevice("")
          }
        }}
        open={activeDialog === "attach"}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Attach Volume</AlertDialogTitle>
            <AlertDialogDescription>
              Attach this volume to a running instance.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <Field>
            <FieldTitle>
              <label htmlFor="attachInstance">Instance</label>
            </FieldTitle>
            <Select
              onValueChange={(value) => setAttachInstanceId(value ?? "")}
              value={attachInstanceId}
            >
              <SelectTrigger className="w-full" id="attachInstance">
                <SelectValue placeholder="Select an instance" />
              </SelectTrigger>
              <SelectContent>
                {runningInstances.map((instance) => {
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
              onChange={(e) => setAttachDevice(e.target.value)}
              placeholder="/dev/sdf"
              value={attachDevice}
            />
          </Field>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={attachMutation.isPending || !attachInstanceId}
              onClick={handleAttach}
            >
              {attachMutation.isPending ? "Attaching\u2026" : "Attach"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog
        onOpenChange={(open) => {
          if (!open) {
            closeDialog()
            setDetachForce(false)
          }
        }}
        open={activeDialog === "detach"}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Detach Volume</AlertDialogTitle>
            <AlertDialogDescription>
              Are you sure you want to detach the volume &quot;{volume.VolumeId}
              &quot; from instance &quot;{volume.Attachments?.[0]?.InstanceId}
              &quot;?
            </AlertDialogDescription>
          </AlertDialogHeader>
          <label className="flex items-center gap-2 text-sm">
            <input
              aria-label="Force detach"
              checked={detachForce}
              onChange={(e) => setDetachForce(e.target.checked)}
              type="checkbox"
            />
            Force detach
          </label>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={detachMutation.isPending}
              onClick={handleDetach}
            >
              {detachMutation.isPending ? "Detaching\u2026" : "Detach"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
