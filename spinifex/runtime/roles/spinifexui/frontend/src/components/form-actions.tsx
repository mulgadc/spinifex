import { LoaderCircle } from "lucide-react"

import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

// Both labels share one grid cell, so the button is as wide as the longer and
// keeps that width while they cross-fade.
const LAYER =
  "col-start-1 row-start-1 inline-flex items-center justify-center gap-1 transition-opacity duration-150 ease-out motion-reduce:transition-none"

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

// The submit button cross-fades to a spinner with the pending label in place,
// so neither it nor anything beside it moves while the request is in flight.
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
          <span className="grid">
            <span
              aria-hidden={disabled}
              className={cn(LAYER, disabled && "opacity-0")}
              data-label="submit"
            >
              {submitLabel}
            </span>
            <span
              aria-hidden={!disabled}
              className={cn(LAYER, !disabled && "opacity-0")}
              data-label="pending"
            >
              <LoaderCircle
                aria-hidden
                className={cn(
                  "motion-reduce:animate-none",
                  disabled && "animate-spin",
                )}
              />
              {pendingLabel}
            </span>
          </span>
        </Button>
      </div>
    </div>
  )
}
