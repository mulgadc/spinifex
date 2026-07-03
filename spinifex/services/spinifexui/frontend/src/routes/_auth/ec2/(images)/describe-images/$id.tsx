import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { ArrowUpCircle, Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
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
import { useAdmin } from "@/contexts/admin-context"
import { isSystemManagedImage } from "@/lib/system-managed"
import { usePromoteImage } from "@/mutations/admin"
import { useDeregisterImage } from "@/mutations/ec2"
import { ec2ImageQueryOptions } from "@/queries/ec2"

import { AmiDetails } from "../../-components/ami-details"

const SYSTEM_OWNER_ID = "000000000000"

export const Route = createFileRoute("/_auth/ec2/(images)/describe-images/$id")(
  {
    loader: async ({ context, params }) =>
      await context.queryClient.ensureQueryData(
        ec2ImageQueryOptions(params.id),
      ),
    head: ({ loaderData }) => ({
      meta: [
        {
          title: `${loaderData?.Images?.[0]?.Name ?? "Image"} | EC2 | Mulga`,
        },
      ],
    }),
    component: ImageDetail,
  },
)

function ImageDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2ImageQueryOptions(id))
  const image = data?.Images?.[0]
  const { isAdmin } = useAdmin()
  const deregisterMutation = useDeregisterImage()
  const promoteMutation = usePromoteImage()
  const [showDeregisterDialog, setShowDeregisterDialog] = useState(false)
  const [showPromoteDialog, setShowPromoteDialog] = useState(false)

  if (!image?.ImageId || isSystemManagedImage(image)) {
    return (
      <>
        <BackLink to="/ec2/describe-images">Back to images</BackLink>
        <p className="text-muted-foreground">Image not found.</p>
      </>
    )
  }

  const isSystemImage = image.OwnerId === SYSTEM_OWNER_ID

  async function handleDeregister() {
    try {
      await deregisterMutation.mutateAsync(id)
      navigate({ to: "/ec2/describe-images" })
    } finally {
      setShowDeregisterDialog(false)
    }
  }

  async function handlePromote() {
    try {
      await promoteMutation.mutateAsync(id)
      setShowPromoteDialog(false)
    } catch {
      // error shown via promoteMutation.error
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-images">Back to images</BackLink>

      {deregisterMutation.error && (
        <ErrorBanner
          error={deregisterMutation.error}
          msg="Failed to deregister image"
        />
      )}

      {promoteMutation.error && (
        <ErrorBanner
          error={promoteMutation.error}
          msg="Failed to promote image"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              {isAdmin && !isSystemImage && (
                <Button
                  onClick={() => setShowPromoteDialog(true)}
                  size="sm"
                  variant="outline"
                >
                  <ArrowUpCircle className="size-4" />
                  Promote to System
                </Button>
              )}
              <Button
                onClick={() => setShowDeregisterDialog(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Deregister
              </Button>
              <StateBadge state={image.State} />
            </div>
          }
          subtitle="Image Details"
          title={image.ImageId}
        />

        <AmiDetails image={image} showExtendedDetails />
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to deregister "${image.ImageId}"? The backing snapshot will not be deleted.`}
        isPending={deregisterMutation.isPending}
        onConfirm={handleDeregister}
        onOpenChange={setShowDeregisterDialog}
        open={showDeregisterDialog}
        title="Deregister Image"
      />

      <AlertDialog onOpenChange={setShowPromoteDialog} open={showPromoteDialog}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Promote to System Image</AlertDialogTitle>
            <AlertDialogDescription>
              This will make &ldquo;{image.ImageId}&rdquo; visible to all
              accounts as a system image. This cannot be undone via the UI.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={promoteMutation.isPending}
              onClick={handlePromote}
            >
              {promoteMutation.isPending ? "Promoting…" : "Promote"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
