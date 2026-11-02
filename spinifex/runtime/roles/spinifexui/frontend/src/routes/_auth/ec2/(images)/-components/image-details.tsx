import type { Image } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import {
  ArrowUpCircle,
  Eye,
  Maximize2,
  SlidersHorizontal,
  Trash2,
} from "lucide-react"
import { useState } from "react"

import { DetailPanel, PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Ec2Tags } from "@/components/resource-tags"
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
import { useAdmin } from "@/hooks/use-admin"
import type { PanelTab } from "@/lib/panel-search"
import { usePromoteImage } from "@/mutations/admin"
import { useDeregisterImage } from "@/mutations/ec2"

import { AmiDetails } from "../../-components/ami-details"

const SYSTEM_OWNER_ID = "000000000000"

function Settings({
  image,
  onDeregistered,
}: {
  image: Image
  onDeregistered: () => void
}) {
  const id = image.ImageId ?? ""
  const { isAdmin } = useAdmin()
  const deregister = useDeregisterImage()
  const promote = usePromoteImage()
  const [confirmDeregister, setConfirmDeregister] = useState(false)
  const [confirmPromote, setConfirmPromote] = useState(false)
  const isSystemImage = image.OwnerId === SYSTEM_OWNER_ID

  async function handlePromote() {
    try {
      await promote.mutateAsync(id)
      setConfirmPromote(false)
    } catch {
      // Shown by the banner above the actions.
    }
  }

  return (
    <>
      <Ec2Tags
        id={image.ImageId}
        invalidate={["ec2", "images"]}
        readOnly={isSystemImage}
        tags={image.Tags}
      />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {promote.error && (
            <ErrorBanner error={promote.error} msg="Failed to promote image" />
          )}
          {isAdmin && (
            <Button
              disabled={isSystemImage || promote.isPending}
              onClick={() => {
                setConfirmPromote(true)
              }}
              size="sm"
              variant="outline"
            >
              <ArrowUpCircle data-icon="inline-start" />
              Promote to system
            </Button>
          )}
          <Button
            disabled={deregister.isPending}
            onClick={() => {
              setConfirmDeregister(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Deregister image
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Are you sure you want to deregister "${id}"? The backing snapshot will not be deleted.`}
        identifier={id}
        mutation={deregister}
        onDeleted={onDeregistered}
        onOpenChange={setConfirmDeregister}
        open={confirmDeregister}
        title="Deregister Image"
      />
      <AlertDialog onOpenChange={setConfirmPromote} open={confirmPromote}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Promote to System Image</AlertDialogTitle>
            <AlertDialogDescription>
              This will make &ldquo;{id}&rdquo; visible to all accounts as a
              system image. This cannot be undone via the UI.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={promote.isPending}
              onClick={() => {
                void handlePromote()
              }}
            >
              {promote.isPending ? "Promoting…" : "Promote"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

interface ImageDetailsProps {
  image: Image
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a deregister.
  onClose: () => void
  layout?: "auto" | "page"
}

// One image's details, as the list's side panel or as its own page.
export function ImageDetails({
  image,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: ImageDetailsProps) {
  const id = image.ImageId ?? ""

  return (
    <DetailPanel
      closeLabel={layout === "page" ? "Back to images" : "Close image details"}
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open image page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-images/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Image details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={image.Name ? id : undefined}
      summary={<StateBadge state={image.State} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <AmiDetails image={image} showExtendedDetails />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings image={image} onDeregistered={onClose} />,
        },
      ]}
      title={image.Name ?? id}
    />
  )
}
