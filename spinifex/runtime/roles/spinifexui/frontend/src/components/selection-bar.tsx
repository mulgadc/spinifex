import { X } from "lucide-react"
import type { ReactNode } from "react"

import { Button } from "@/components/ui/button"

interface SelectionBarProps {
  count: number
  onClear: () => void
  // The bulk actions, usually ending with a destructive one.
  children: ReactNode
  // Shown on its own line beneath the actions.
  footer?: ReactNode
}

// Shown above a list once rows are selected: how many, what can be done to
// them, and a way out.
export function SelectionBar({
  count,
  onClear,
  children,
  footer,
}: SelectionBarProps) {
  return (
    <div
      aria-label="Selection actions"
      className="mb-3 flex flex-wrap items-center gap-2 rounded-lg border border-primary/30 bg-primary/5 px-3 py-2"
      role="toolbar"
    >
      <span className="mr-auto text-sm font-medium">{count} selected</span>
      {children}
      <Button
        aria-label="Clear selection"
        onClick={onClear}
        size="icon-sm"
        variant="ghost"
      >
        <X />
      </Button>
      {footer && <div className="basis-full">{footer}</div>}
    </div>
  )
}
