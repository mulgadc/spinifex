import type { Event } from "@aws-sdk/client-rds"

import { formatDateTime } from "@/lib/utils"

interface RdsEventsPanelProps {
  events: Event[]
}

// The event ring an instance and a snapshot each keep. Both render it the same
// way, so the table lives here rather than once per detail page.
export function RdsEventsPanel({ events }: RdsEventsPanelProps) {
  if (events.length === 0) {
    return (
      <p className="text-muted-foreground">No events in the last 14 days.</p>
    )
  }

  return (
    <div className="overflow-x-auto rounded-lg border bg-card">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-left text-muted-foreground">
            <th className="px-4 py-2 font-medium">Time</th>
            <th className="px-4 py-2 font-medium">Categories</th>
            <th className="px-4 py-2 font-medium">Message</th>
          </tr>
        </thead>
        <tbody>
          {events
            .toSorted(
              (a, b) => (b.Date?.getTime() ?? 0) - (a.Date?.getTime() ?? 0),
            )
            .map((event) => (
              <tr
                className="border-b last:border-0"
                key={`${event.Date?.toISOString()}-${event.Message}`}
              >
                <td className="px-4 py-2 font-mono text-xs">
                  {formatDateTime(event.Date)}
                </td>
                <td className="px-4 py-2 text-xs">
                  {event.EventCategories?.join(", ")}
                </td>
                <td className="px-4 py-2">{event.Message}</td>
              </tr>
            ))}
        </tbody>
      </table>
    </div>
  )
}
