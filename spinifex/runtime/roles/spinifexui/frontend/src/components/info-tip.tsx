import { Info } from "lucide-react"
import type { ReactNode } from "react"

import { Button } from "@/components/ui/button"
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover"

interface InfoTipProps {
  // Names what the tip explains, for the button's accessible name.
  label: string
  children: ReactNode
}

// UniFi's (i): a small button beside a label that opens a short explanation.
// It opens on hover as well as click, so reading it costs no extra step.
export function InfoTip({ label, children }: InfoTipProps) {
  return (
    <Popover>
      <PopoverTrigger
        aria-label={`About ${label}`}
        delay={150}
        openOnHover
        render={<Button size="icon-xs" variant="ghost" />}
      >
        <Info aria-hidden className="size-3.5" />
      </PopoverTrigger>
      <PopoverContent align="start" side="top">
        <div className="flex flex-col gap-2 text-left">{children}</div>
      </PopoverContent>
    </Popover>
  )
}

interface InfoListProps {
  items: readonly (readonly [string, ReactNode])[]
}

// The facts inside a tip, as label and value rows.
export function InfoList({ items }: InfoListProps) {
  return (
    <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
      {items.map(([term, value]) => (
        <div className="contents" key={term}>
          <dt className="text-muted-foreground">{term}</dt>
          <dd className="min-w-0 break-words">{value ?? "—"}</dd>
        </div>
      ))}
    </dl>
  )
}
