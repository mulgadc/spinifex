import type { ReactNode } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"

// Structural rather than the mutation's own type: the three deletes return
// different payloads and this only ever reads the four fields below.
interface DeleteMutation {
  error: Error | null
  isPending: boolean
  mutateAsync: (identifier: string) => Promise<unknown>
  reset: () => void
}

interface RdsDeleteDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  identifier: string
  title: string
  description: ReactNode
  mutation: DeleteMutation
  onDeleted?: () => void
}

// Every RDS delete that can be refused shares this shape: the dialog stays open
// on failure so the refusal — which names what still holds the resource — is
// readable rather than lost behind a closing dialog.
export function RdsDeleteDialog({
  open,
  onOpenChange,
  identifier,
  title,
  description,
  mutation,
  onDeleted,
}: RdsDeleteDialogProps) {
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
      isPending={mutation.isPending}
      onConfirm={() => void handleDelete()}
      onOpenChange={handleOpenChange}
      open={open}
      title={title}
    />
  )
}
