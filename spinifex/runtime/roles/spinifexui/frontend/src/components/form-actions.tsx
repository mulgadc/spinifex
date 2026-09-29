import { Button } from "@/components/ui/button"

interface FormActionsProps {
  isSubmitting: boolean
  isPending: boolean
  onCancel: () => void
  submitLabel: string
  pendingLabel: string
}

export function FormActions({
  isSubmitting,
  isPending,
  onCancel,
  submitLabel,
  pendingLabel,
}: FormActionsProps) {
  const disabled = isSubmitting || isPending
  return (
    <div className="flex gap-2">
      <Button
        disabled={disabled}
        onClick={onCancel}
        type="button"
        variant="outline"
      >
        Cancel
      </Button>
      <Button disabled={disabled} type="submit">
        {disabled ? pendingLabel : submitLabel}
      </Button>
    </div>
  )
}
