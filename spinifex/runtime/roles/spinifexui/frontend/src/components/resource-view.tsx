import type { ReactNode } from "react"

import { useWideLayout } from "@/hooks/use-wide-layout"

interface ResourceViewProps {
  // The list page: heading, filters and table.
  children: ReactNode
  // The open detail or create panel, if any.
  panel?: ReactNode
}

// A list with its panel. From lg up the panel slides over the list's right edge
// and the list keeps its width beneath it; below lg the panel replaces it.
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
    <>
      {children}
      {panel}
    </>
  )
}
