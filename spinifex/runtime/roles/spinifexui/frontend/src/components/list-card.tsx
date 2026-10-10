import type { LinkProps } from "@tanstack/react-router"
import { Link } from "@tanstack/react-router"
import type { ReactNode } from "react"

interface ListCardProps extends Omit<LinkProps, "children"> {
  title: string
  subtitle?: string | ReactNode
  badge?: ReactNode
  children?: ReactNode
}

export function ListCard({
  title,
  subtitle,
  badge,
  children,
  ...linkProps
}: ListCardProps) {
  return (
    <Link
      className="block rounded-lg border bg-card p-4 transition-colors hover:bg-accent"
      {...linkProps}
    >
      <div className="flex items-start justify-between">
        <div className="flex-1">
          <h3 className="font-semibold">{title}</h3>
          {subtitle && (
            <p className="mt-1 text-sm font-medium text-muted-foreground">
              {subtitle}
            </p>
          )}
          {children}
        </div>
        {badge && <div className="ml-4">{badge}</div>}
      </div>
    </Link>
  )
}
