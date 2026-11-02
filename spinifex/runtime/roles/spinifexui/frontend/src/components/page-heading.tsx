import type { ReactNode } from "react"

interface PageHeadingProps {
  title: string
  subtitle?: string
  actions?: ReactNode
}

// Actions wrap below the title on narrow screens rather than squeezing it.
export function PageHeading({ title, subtitle, actions }: PageHeadingProps) {
  return (
    <div className="mb-6 flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
      <div className="min-w-0">
        <h1 className="text-2xl font-semibold tracking-tight break-words">
          {title}
        </h1>
        {subtitle && <p className="mt-1 text-muted-foreground">{subtitle}</p>}
      </div>
      {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
    </div>
  )
}
