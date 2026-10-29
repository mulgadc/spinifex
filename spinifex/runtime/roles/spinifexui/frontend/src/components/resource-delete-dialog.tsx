import type { ReactNode } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"

// Structural rather than a mutation's own type: deletes return different
// payloads and this only ever reads the four fields below.
export interface DeleteMutation<TIdentifier = string> {
  error: Error | null
  isPending: boolean
  // oxlint-disable-next-line anti-slop/no-unknown-returns -- the dialog awaits the delete and never reads its payload
  mutateAsync: (identifier: TIdentifier) => Promise<unknown>
  reset: () => void
}

interface ResourceDeleteDialogProps<TIdentifier> {
  open: boolean
  onOpenChange: (open: boolean) => void
  identifier: TIdentifier
  title: string
  description: ReactNode
  mutation: DeleteMutation<TIdentifier>
  onDeleted?: () => void
  confirmLabel?: string
  pendingLabel?: string
}

// The dialog stays open on failure so the refusal — which usually names what
// still holds the resource — is readable rather than lost behind a closing
// dialog. Closing it resets the mutation so a stale error never reappears.
export function ResourceDeleteDialog<TIdentifier>({
  open,
  onOpenChange,
  identifier,
  title,
  description,
  mutation,
  onDeleted,
  confirmLabel,
  pendingLabel,
}: ResourceDeleteDialogProps<TIdentifier>) {
  function handleOpenChange(nextOpen: boolean) {
    if (!nextOpen) {
      mutation.reset()
    }

    onOpenChange(nextOpen)
  }

  async function handleDelete() {
    try {
      await mutation.mutateAsync(identifier)
      handleOpenChange(false)
      onDeleted?.()
    } catch {
      // Left open so the refusal below stays readable.
    }
  }

  return (
    <DeleteConfirmationDialog
      description={
        <>
          {description}
          {mutation.error && (
            <span className="mt-2 block text-destructive">
              {mutation.error.message}
            </span>
          )}
        </>
      }
      confirmLabel={confirmLabel}
      isPending={mutation.isPending}
      onConfirm={() => {
        void handleDelete()
      }}
      onOpenChange={handleOpenChange}
      open={open}
      pendingLabel={pendingLabel}
      title={title}
    />
  )
}
