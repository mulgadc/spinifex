import { type StateTone, StatusBadge } from "@/components/state-badge"
import type { ActivationState } from "@/lib/route53-activation"
import { ACTIVATION_LABELS } from "@/types/route53"

const STATE_TONES: Record<ActivationState, StateTone> = {
  ACTIVE: "success",
  PENDING_NAMESERVERS: "progress",
  NAMESERVERS_LOST: "danger",
  SUSPENDED: "muted",
}

export function ActivationBadge({
  state,
  fallback = "unknown",
}: {
  state: ActivationState | undefined
  fallback?: string
}) {
  return (
    <StatusBadge tone={state ? STATE_TONES[state] : "neutral"}>
      <span className="whitespace-nowrap">
        {state ? ACTIVATION_LABELS[state] : fallback}
      </span>
    </StatusBadge>
  )
}
