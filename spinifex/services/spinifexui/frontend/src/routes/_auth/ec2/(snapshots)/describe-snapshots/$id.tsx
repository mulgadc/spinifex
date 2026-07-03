import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { Copy, Image, Trash2 } from "lucide-react"
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
import { Button } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { formatDateTime } from "@/lib/utils"
import {
  useCopySnapshot,
  useDeleteSnapshot,
  useRegisterImage,
} from "@/mutations/ec2"
import { ec2RegionsQueryOptions, ec2SnapshotQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(snapshots)/describe-snapshots/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2SnapshotQueryOptions(params.id)),
      context.queryClient.ensureQueryData(ec2RegionsQueryOptions),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | EC2 | Mulga`,
      },
    ],
  }),
  component: SnapshotDetail,
})

function SnapshotDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2SnapshotQueryOptions(id))
  const { data: regionsData } = useSuspenseQuery(ec2RegionsQueryOptions)
  const snapshot = data.Snapshots?.[0]
  const deleteMutation = useDeleteSnapshot()
  const copyMutation = useCopySnapshot()
  const registerMutation = useRegisterImage()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)
  const [showCopyDialog, setShowCopyDialog] = useState(false)
  const [copyDescription, setCopyDescription] = useState("")
  const [showRegisterDialog, setShowRegisterDialog] = useState(false)
  const [registerName, setRegisterName] = useState("")
  const [registerDescription, setRegisterDescription] = useState("")
  const [registeredImageId, setRegisteredImageId] = useState<string | null>(
    null,
  )

  const currentRegion = regionsData.Regions?.[0]?.RegionName ?? "ap-southeast-2"

  const canDelete = snapshot?.State === "completed"
  const canRegister = snapshot?.State === "completed"

  function handleRegisterClose(nextOpen: boolean) {
    if (!nextOpen) {
      setRegisterName("")
      setRegisterDescription("")
      setRegisteredImageId(null)
    }
    setShowRegisterDialog(nextOpen)
  }

  async function handleRegister() {
    try {
      const result = await registerMutation.mutateAsync({
        snapshotId: id,
        name: registerName,
        description: registerDescription || undefined,
      })
      setRegisteredImageId(result.ImageId ?? null)
      setRegisterName("")
      setRegisterDescription("")
    } catch {
      // error shown via registerMutation.error
    }
  }

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(id)
      navigate({ to: "/ec2/describe-snapshots" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const handleCopy = async () => {
    try {
      await copyMutation.mutateAsync({
        sourceSnapshotId: id,
        sourceRegion: currentRegion,
        description: copyDescription || undefined,
      })
      setShowCopyDialog(false)
      setCopyDescription("")
    } catch {
      // error is shown via copyMutation.error
    }
  }

  if (!snapshot?.SnapshotId) {
    return (
      <>
        <BackLink to="/ec2/describe-snapshots">Back to snapshots</BackLink>
        <p className="text-muted-foreground">Snapshot not found.</p>
      </>
    )
  }

  const startTime = formatDateTime(snapshot.StartTime)

  return (
    <>
      <BackLink to="/ec2/describe-snapshots">Back to snapshots</BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete snapshot"
        />
      )}

      {copyMutation.error && (
        <ErrorBanner error={copyMutation.error} msg="Failed to copy snapshot" />
      )}

      {registerMutation.error && (
        <ErrorBanner
          error={registerMutation.error}
          msg="Failed to register image"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                disabled={!canDelete}
                onClick={() => setShowDeleteDialog(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
              <Button
                disabled={!canDelete}
                onClick={() => setShowCopyDialog(true)}
                size="sm"
                variant="outline"
              >
                <Copy className="size-4" />
                Copy Snapshot
              </Button>
              <Button
                disabled={!canRegister}
                onClick={() => setShowRegisterDialog(true)}
                size="sm"
                variant="outline"
              >
                <Image className="size-4" />
                Register as AMI
              </Button>
              <StateBadge state={snapshot.State} />
            </div>
          }
          subtitle="Snapshot Details"
          title={snapshot.SnapshotId}
        />

        <DetailCard>
          <DetailCard.Header>Snapshot Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Snapshot ID" value={snapshot.SnapshotId} />
            <DetailRow
              label="Volume ID"
              value={
                snapshot.VolumeId ? (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: snapshot.VolumeId }}
                    to="/ec2/describe-volumes/$id"
                  >
                    {snapshot.VolumeId}
                  </Link>
                ) : undefined
              }
            />
            <DetailRow
              label="Volume Size"
              value={`${snapshot.VolumeSize} GiB`}
            />
            <DetailRow label="State" value={snapshot.State} />
            <DetailRow label="Description" value={snapshot.Description} />
            <DetailRow label="Progress" value={snapshot.Progress} />
            <DetailRow label="Start Time" value={startTime} />
            <DetailRow label="Owner ID" value={snapshot.OwnerId} />
            <DetailRow
              label="Encrypted"
              value={snapshot.Encrypted ? "Yes" : "No"}
            />
          </DetailCard.Content>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the snapshot "${snapshot.SnapshotId}"? This action cannot be undone.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Snapshot"
      />

      <AlertDialog onOpenChange={setShowCopyDialog} open={showCopyDialog}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Copy Snapshot</AlertDialogTitle>
            <AlertDialogDescription>
              Create a copy of this snapshot in the current region.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <Field>
            <FieldTitle>
              <label htmlFor="copyDescription">Description (optional)</label>
            </FieldTitle>
            <Input
              id="copyDescription"
              onChange={(e) => setCopyDescription(e.target.value)}
              placeholder="Description for the snapshot copy"
              value={copyDescription}
            />
          </Field>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={copyMutation.isPending}
              onClick={handleCopy}
            >
              {copyMutation.isPending ? "Copying\u2026" : "Copy Snapshot"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog onOpenChange={handleRegisterClose} open={showRegisterDialog}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {registeredImageId ? "Image Registered" : "Register as AMI"}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {registeredImageId ? (
                <>
                  AMI{" "}
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: registeredImageId }}
                    to="/ec2/describe-images/$id"
                  >
                    {registeredImageId}
                  </Link>{" "}
                  was registered successfully.
                </>
              ) : (
                `Create an AMI from snapshot "${id}".`
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {!registeredImageId && (
            <>
              <Field>
                <FieldTitle>
                  <label htmlFor="registerName">Name</label>
                </FieldTitle>
                <Input
                  id="registerName"
                  onChange={(e) => setRegisterName(e.target.value)}
                  placeholder="my-image"
                  value={registerName}
                />
              </Field>
              <Field>
                <FieldTitle>
                  <label htmlFor="registerDescription">
                    Description (optional)
                  </label>
                </FieldTitle>
                <Textarea
                  id="registerDescription"
                  onChange={(e) => setRegisterDescription(e.target.value)}
                  placeholder="Optional description"
                  rows={2}
                  value={registerDescription}
                />
              </Field>
            </>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>
              {registeredImageId ? "Close" : "Cancel"}
            </AlertDialogCancel>
            {!registeredImageId && (
              <AlertDialogAction
                disabled={registerMutation.isPending || !registerName.trim()}
                onClick={handleRegister}
              >
                {registerMutation.isPending
                  ? "Registering\u2026"
                  : "Register AMI"}
              </AlertDialogAction>
            )}
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
