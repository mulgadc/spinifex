import { cn } from "@/lib/utils"

// "Complete" is the only status an RDS subnet group ever reports, and it is
// capitalised where every other settled state is not.
const SUCCESS_STATES = new Set([
  "running",
  "available",
  "completed",
  "Complete",
  "Active",
  // Bedrock guardrails, knowledge bases, data sources and ingestion jobs.
  "ACTIVE",
  "AVAILABLE",
  "COMPLETE",
  "READY",
  // ELBv2 target health.
  "healthy",
])

// "stopped" stays an error state because EC2 reads it that way. RDS treats a
// stopped instance as a normal resting state, so its badge is redder than the
// state warrants; splitting the map by service is its own change.
const ERROR_STATES = new Set([
  "stopped",
  "error",
  "failed",
  "FAILED",
  "DELETE_UNSUCCESSFUL",
  "UPDATE_UNSUCCESSFUL",
  "unhealthy",
  "unavailable",
])

const WARNING_STATES = new Set([
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
  "CREATING",
  "DELETING",
  "IN_PROGRESS",
  "STARTING",
  "STOPPING",
  "UPDATING",
  "VERSIONING",
  "initial",
  "draining",
])

type StateTone = "success" | "danger" | "warning" | "muted" | "neutral"

const TONE_CLASSES: Record<StateTone, string> = {
  success: "bg-success-subtle text-success-foreground",
  danger: "bg-danger-subtle text-danger-foreground",
  warning: "bg-warning-subtle text-warning-foreground",
  muted: "bg-neutral-subtle text-muted-foreground",
  neutral: "bg-neutral-subtle text-foreground",
}

function getStateTone(state: string | undefined): StateTone {
  if (state && SUCCESS_STATES.has(state)) {
    return "success"
  }

  if (state && ERROR_STATES.has(state)) {
    return "danger"
  }

  if (state && WARNING_STATES.has(state)) {
    return "warning"
  }

  if (state === "terminated") {
    return "muted"
  }

  return "neutral"
}

export function StateBadge({ state }: { state: string | undefined }) {
  const tone = getStateTone(state)

  return (
    <div
      className={cn(
        "inline-flex w-fit rounded-full px-2 py-1 text-xs",
        TONE_CLASSES[tone],
      )}
      data-tone={tone}
    >
      {state}
    </div>
  )
}
