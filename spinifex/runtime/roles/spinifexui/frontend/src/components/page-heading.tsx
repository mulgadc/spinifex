import type { ReactNode } from "react"

interface PageHeadingProps {
  title: string
  subtitle?: string
  actions?: ReactNode
}

export function PageHeading({ title, subtitle, actions }: PageHeadingProps) {
  return (
    <div className="mb-6 flex items-start justify-between">
      <div>
        <h1 className="text-3xl">{title}</h1>
        {subtitle && <p className="mt-1 text-muted-foreground">{subtitle}</p>}
      </div>
      {actions && <div>{actions}</div>}
    </div>
  )
}
