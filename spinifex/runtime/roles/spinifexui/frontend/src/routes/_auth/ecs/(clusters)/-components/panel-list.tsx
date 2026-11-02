import type { ReactNode } from "react"

export function PanelList({ children }: { children: ReactNode }) {
  return <ul className="divide-y text-sm">{children}</ul>
}

interface PanelListItemProps {
  // The item's name, usually a link to its own page.
  title: ReactNode
  detail?: ReactNode
  // The state badge, on the right of the title.
  state?: ReactNode
  actions?: ReactNode
}

// One row of a list inside a panel section: a cluster's services, tasks or
// container instances, which are too wide for a table in a narrow column.
export function PanelListItem({
  title,
  detail,
  state,
  actions,
}: PanelListItemProps) {
  return (
    <li className="flex flex-col gap-1.5 py-2">
      <div className="flex items-center justify-between gap-3">
        <span className="min-w-0 truncate">{title}</span>
        {state}
      </div>
      {detail && (
        <div className="text-xs text-muted-foreground tabular-nums">
          {detail}
        </div>
      )}
      {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
    </li>
  )
}
