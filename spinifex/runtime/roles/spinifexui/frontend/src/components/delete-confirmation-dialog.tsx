import { type ReactNode, useState } from "react"

import { ConfirmPhrase, phraseConfirmed } from "@/components/confirm-phrase"
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

interface DeleteConfirmationDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description: ReactNode
  isPending: boolean
  onConfirm: () => void
  confirmLabel?: string
  pendingLabel?: string
  // Typed before the confirm enables, for deletes that cannot be walked back.
  confirmPhrase?: string
}

export function DeleteConfirmationDialog({
  open,
  onOpenChange,
  title,
  description,
  isPending,
  onConfirm,
  confirmLabel = "Delete",
  pendingLabel = "Deleting\u2026",
  confirmPhrase,
}: DeleteConfirmationDialogProps) {
  const [typed, setTyped] = useState("")
  const [shownOpen, setShownOpen] = useState(open)

  // However the dialog closes, it asks afresh next time.
  if (open !== shownOpen) {
    setShownOpen(open)
    setTyped("")
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        {confirmPhrase && (
          <ConfirmPhrase
            onChange={setTyped}
            phrase={confirmPhrase}
            value={typed}
          />
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={isPending || !phraseConfirmed(confirmPhrase, typed)}
            onClick={onConfirm}
          >
            {isPending ? pendingLabel : confirmLabel}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
