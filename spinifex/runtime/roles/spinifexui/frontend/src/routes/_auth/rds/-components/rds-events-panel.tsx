import type { Event } from "@aws-sdk/client-rds"

import { formatDateTime } from "@/lib/utils"

interface RdsEventsPanelProps {
  events: Event[]
}

// The event ring an instance and a snapshot each keep, newest first. Stacked
// rather than tabled, so it reads in a side panel's narrow column.
export function RdsEventsPanel({ events }: RdsEventsPanelProps) {
  if (events.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        No events in the last 14 days.
      </p>
    )
  }

  return (
    <ol aria-label="Events" className="divide-y text-sm">
      {events
        .toSorted((a, b) => (b.Date?.getTime() ?? 0) - (a.Date?.getTime() ?? 0))
        .map((event) => (
          <li
            className="flex flex-col gap-0.5 py-2"
            key={`${event.Date?.toISOString()}-${event.Message}`}
          >
            <span className="flex flex-wrap items-baseline justify-between gap-x-3 text-xs text-muted-foreground">
              <span className="font-mono">{formatDateTime(event.Date)}</span>
              <span>{event.EventCategories?.join(", ")}</span>
            </span>
            <span>{event.Message}</span>
          </li>
        ))}
    </ol>
  )
}
