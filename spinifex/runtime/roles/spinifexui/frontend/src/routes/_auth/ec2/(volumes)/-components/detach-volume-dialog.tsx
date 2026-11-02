import { useState } from "react"

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
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { useDetachVolume } from "@/mutations/ec2"

// Detaching confirms in a dialog, with AWS's force option beside it.
export function DetachVolumeDialog({
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
