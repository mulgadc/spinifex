import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import { ArrowUpCircle } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteResourceButton } from "@/components/delete-resource-button"
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
import { useAdmin } from "@/hooks/use-admin"
import { isSystemManagedImage } from "@/lib/system-managed"
import { usePromoteImage } from "@/mutations/admin"
import { useDeregisterImage } from "@/mutations/ec2"
import { ec2ImageQueryOptions } from "@/queries/ec2"

import { AmiDetails } from "../../-components/ami-details"

const SYSTEM_OWNER_ID = "000000000000"

export const Route = createFileRoute("/_auth/ec2/(images)/describe-images/$id")(
  {
    loader: async ({ context, params }) =>
      await context.queryClient.query({
        ...ec2ImageQueryOptions(params.id),
        staleTime: "static",
      }),
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
  const { data } = useSuspenseQuery(ec2ImageQueryOptions(id))
  const image = data?.Images?.[0]
  const { isAdmin } = useAdmin()
  const deregisterMutation = useDeregisterImage()
  const promoteMutation = usePromoteImage()
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
                  onClick={() => {
                    setShowPromoteDialog(true)
                  }}
                  size="sm"
                  variant="outline"
                >
                  <ArrowUpCircle className="size-4" />
                  Promote to System
                </Button>
              )}
              <DeleteResourceButton
                description={`Are you sure you want to deregister "${image.ImageId}"? The backing snapshot will not be deleted.`}
                id={id}
                label="Deregister"
                mutation={deregisterMutation}
                title="Deregister Image"
                to="/ec2/describe-images"
              />
              <StateBadge state={image.State} />
            </div>
          }
          subtitle="Image Details"
          title={image.ImageId}
        />

        <AmiDetails image={image} showExtendedDetails />
      </div>

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
