import type { ActivationState } from "@/lib/route53-activation"
import { cn } from "@/lib/utils"
import { ACTIVATION_LABELS } from "@/types/route53"

const STATE_CLASSES: Record<ActivationState, string> = {
  ACTIVE: "border-success bg-success/10 text-success-foreground",
  PENDING_NAMESERVERS: "border-warning bg-warning/10 text-warning-foreground",
  NAMESERVERS_LOST: "border-destructive bg-destructive/10 text-destructive",
  SUSPENDED: "border-border bg-muted text-muted-foreground",
}

const UNKNOWN_CLASS = "border-border bg-muted text-muted-foreground"

export function ActivationBadge({
  state,
  fallback = "Unknown",
}: {
  state: ActivationState | undefined
  fallback?: string
}) {
  return (
    <span
      className={cn(
        "inline-block rounded-full border px-2 py-1 text-xs whitespace-nowrap",
        state ? STATE_CLASSES[state] : UNKNOWN_CLASS,
      )}
    >
      {state ? ACTIVATION_LABELS[state] : fallback}
    </span>
  )
}
