import { LoaderCircle } from "lucide-react"
import type { ReactNode } from "react"

import { cn } from "@/lib/utils"

// States are matched after normalising, so EKS's ACTIVE, RDS's Complete and
// EC2's running read the same way and land in the same tone.
const SUCCESS_STATES = new Set([
  "running",
  "available",
  "completed",
  "complete",
  "active",
  "ready",
  "healthy",
  "associated",
  "attached",
  "in-use",
  "ok",
  "main",
  "enabled",
  "issued",
  "succeeded",
  "inservice",
])

// "stopped" stays an error state because EC2 reads it that way. RDS treats a
// stopped instance as a normal resting state, so its badge is redder than the
// state warrants; splitting the map by service is its own change.
const DANGER_STATES = new Set([
  "stopped",
  "error",
  "failed",
  "create-failed",
  "delete-failed",
  "delete-unsuccessful",
  "update-unsuccessful",
  "update-failed",
  "blackhole",
  "unhealthy",
  "unavailable",
])

// Orange: the resource works but needs attention.
const CAUTION_STATES = new Set([
  "degraded",
  "impaired",
  "warning",
  "insufficient-data",
  "unused",
  "expired",
])

// Yellow: the resource is changing state and will settle on its own.
const PROGRESS_STATES = new Set([
  "pending",
  "shutting-down",
  "stopping",
  "creating",
  "modifying",
  "backing-up",
  "rebooting",
  "starting",
  "recovering",
  "deleting",
  "in-progress",
  "updating",
  "versioning",
  "initial",
  "draining",
  "associating",
  "disassociating",
  "attaching",
  "detaching",
  "provisioning",
  "registering",
  "deregistering",
  "pending-validation",
  "optimizing",
])

const MUTED_STATES = new Set(["terminated", "deleted", "inactive", "no"])

export type StateTone =
  | "success"
  | "progress"
  | "caution"
  | "danger"
  | "muted"
  | "neutral"

const TONE_CLASSES: Record<StateTone, string> = {
  success: "bg-success-subtle text-success-foreground",
  progress: "bg-warning-subtle text-warning-foreground",
  caution: "bg-caution-subtle text-caution-foreground",
  danger: "bg-danger-subtle text-danger-foreground",
  muted: "bg-neutral-subtle text-muted-foreground",
  neutral: "bg-neutral-subtle text-foreground",
}

// Lowercase with hyphens, as EC2 writes its states; the API value is untouched.
export function displayState(state: string): string {
  return state.toLowerCase().replaceAll("_", "-")
}

export function getStateTone(state: string | undefined): StateTone {
  // A Bedrock job a user stopped is finished, not failed.
  if (state === undefined || state === "STOPPED") {
    return "neutral"
  }

  const key = displayState(state)

  if (SUCCESS_STATES.has(key)) {
    return "success"
  }

  if (DANGER_STATES.has(key)) {
    return "danger"
  }

  if (CAUTION_STATES.has(key)) {
    return "caution"
  }

  if (PROGRESS_STATES.has(key)) {
    return "progress"
  }

  return MUTED_STATES.has(key) ? "muted" : "neutral"
}

// For a status that is not an API state, such as a cluster's health.
export function StatusBadge({
  tone,
  children,
}: {
  tone: StateTone
  children: ReactNode
}) {
  return (
    <div
      className={cn(
        "inline-flex w-fit items-center gap-1 rounded-full px-2 py-1 text-xs",
        TONE_CLASSES[tone],
      )}
      data-tone={tone}
    >
      {children}
    </div>
  )
}

// A changing state spins the submit button's loader, so a launch reads as work
// still under way rather than a label waiting to change.
export function StateBadge({ state }: { state: string | undefined }) {
  const tone = getStateTone(state)

  return (
    <StatusBadge tone={tone}>
      {tone === "progress" && (
        <LoaderCircle
          aria-hidden
          className="size-3 animate-spin motion-reduce:animate-none"
          data-icon="progress"
        />
      )}
      {state === undefined ? undefined : displayState(state)}
    </StatusBadge>
  )
}
