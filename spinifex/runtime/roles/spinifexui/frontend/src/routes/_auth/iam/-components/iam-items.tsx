import type { ReactNode } from "react"

// The rows of a panel section that lists related resources: policies, members,
// groups, keys. Each names the resource and carries its one action.
export function IamItems({ children }: { children: ReactNode }) {
  return <ul className="divide-y text-sm">{children}</ul>
}

interface IamItemProps {
  children: ReactNode
  // A second line under the name, usually an ARN or ID.
  detail?: ReactNode
  action?: ReactNode
}

export function IamItem({ children, detail, action }: IamItemProps) {
  return (
    <li className="flex items-center justify-between gap-3 py-2">
      <div className="min-w-0">
        <div className="truncate font-medium">{children}</div>
        {detail && (
          <div className="truncate font-mono text-xs text-muted-foreground">
            {detail}
          </div>
        )}
      </div>
      {action && <div className="flex shrink-0 gap-2">{action}</div>}
    </li>
  )
}
