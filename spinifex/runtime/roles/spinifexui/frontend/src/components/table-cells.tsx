import type { ReactNode } from "react"

import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { relativeTime } from "@/lib/relative-time"
import { cn, formatDateTime } from "@/lib/utils"

interface Tag {
  Key?: string
  Value?: string
}

// A short time in the cell and the full one on hover, so a time column takes
// the width of "8 months ago" rather than a whole timestamp.
export function RelativeTime({ date }: { date: Date | string | undefined }) {
  if (!date) {
    return <span className="text-muted-foreground">—</span>
  }

  const full = formatDateTime(date)

  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <time
            className="whitespace-nowrap"
            dateTime={new Date(date).toISOString()}
          />
        }
      >
        {relativeTime(date)}
      </TooltipTrigger>
      <TooltipContent>{full}</TooltipContent>
    </Tooltip>
  )
}

// Ends in an ellipsis rather than widening its column; hover shows it all.
export function TruncatedText({
  text,
  className,
}: {
  text: string | undefined
  className?: string
}) {
  if (!text) {
    return <span className="text-muted-foreground">—</span>
  }

  return (
    <Tooltip>
      <TooltipTrigger render={<span className="block min-w-0" />}>
        <span className={cn("block max-w-48 truncate", className)}>{text}</span>
      </TooltipTrigger>
      <TooltipContent>{text}</TooltipContent>
    </Tooltip>
  )
}

function TagPill({ children }: { children: ReactNode }) {
  return (
    <span className="inline-flex max-w-40 items-center truncate rounded-full border bg-muted px-2 py-0.5 font-mono text-2xs text-foreground">
      {children}
    </span>
  )
}

const NO_KEYS: readonly string[] = []

// One pill and a count of the rest; hover lists every tag as key and value.
export function TagPills({
  tags,
  omit = NO_KEYS,
}: {
  tags: readonly Tag[] | undefined
  omit?: readonly string[]
}) {
  const shown = (tags ?? [])
    .filter((tag) => tag.Key && !omit.includes(tag.Key))
    .toSorted((a, b) => (a.Key ?? "").localeCompare(b.Key ?? ""))

  const [first, ...rest] = shown

  if (!first) {
    return <span className="text-muted-foreground">—</span>
  }

  return (
    <Tooltip>
      <TooltipTrigger render={<span />}>
        <span className="inline-flex items-center gap-1">
          <TagPill>{first.Key}</TagPill>
          {rest.length > 0 && (
            <span className="text-2xs text-muted-foreground">
              +{rest.length}
            </span>
          )}
        </span>
      </TooltipTrigger>
      <TooltipContent>
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-left font-mono text-2xs">
          {shown.map((tag) => (
            <div className="contents" key={tag.Key}>
              <dt className="text-muted-foreground">{tag.Key}</dt>
              <dd className="break-all">{tag.Value || "—"}</dd>
            </div>
          ))}
        </dl>
      </TooltipContent>
    </Tooltip>
  )
}
