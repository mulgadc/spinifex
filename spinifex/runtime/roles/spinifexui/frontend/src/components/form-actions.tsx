import { LoaderCircle } from "lucide-react"

import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"

interface FormActionsProps {
  isSubmitting: boolean
  isPending: boolean
  onCancel: () => void
  submitLabel: string
  pendingLabel: string
  // A failed submit, shown beside the button that sent it.
  error?: Error | null
  errorMsg?: string
  // Greys out submit alone, for a form that cannot be sent yet.
  submitDisabled?: boolean
}

// The submit button turns into a spinner with the pending label in place, so
// nothing above it moves while the request is in flight.
export function FormActions({
  isSubmitting,
  isPending,
  onCancel,
  submitLabel,
  pendingLabel,
  error,
  errorMsg,
  submitDisabled = false,
}: FormActionsProps) {
  const disabled = isSubmitting || isPending

  return (
    <div className="flex flex-col gap-3">
      {error && !disabled && (
        <ErrorBanner className="mb-0" error={error} msg={errorMsg} />
      )}
      <div className="flex gap-2">
        <Button
          disabled={disabled}
          onClick={onCancel}
          type="button"
          variant="outline"
        >
          Cancel
        </Button>
        <Button
          aria-busy={disabled}
          disabled={disabled || submitDisabled}
          type="submit"
        >
          {disabled && (
            <LoaderCircle
              aria-hidden
              className="animate-spin motion-reduce:animate-none"
              data-icon="inline-start"
            />
          )}
          {disabled ? pendingLabel : submitLabel}
        </Button>
      </div>
    </div>
  )
}
