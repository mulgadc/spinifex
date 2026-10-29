import type { ReactNode } from "react"

import { useWideLayout } from "@/hooks/use-wide-layout"

interface ResourceViewProps {
  // The list page: heading, filters and table.
  children: ReactNode
  // The open detail or create panel, if any.
  panel?: ReactNode
}

// A list with its panel. From lg up the panel pushes the list aside rather than
// covering it, so the row it belongs to stays in view; below lg it replaces it.
export function ResourceView({
  children,
  panel,
}: ResourceViewProps): ReactNode {
  const wide = useWideLayout()

  if (!panel) {
    return children
  }

  if (!wide) {
    return panel
  }

  return (
    <div className="flex items-start gap-4">
      <div className="min-w-0 flex-1">{children}</div>
      {panel}
    </div>
  )
}
