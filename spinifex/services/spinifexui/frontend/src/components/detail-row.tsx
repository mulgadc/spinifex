import type { ReactNode } from "react"

export function DetailRow({
  label,
  value,
}: {
  label: string
  value?: ReactNode
}) {
  return (
    <div>
      <dt className="text-sm text-muted-foreground">{label}</dt>
      <dd className="mt-1 font-mono text-sm">
        {value || <span className="text-muted-foreground">—</span>}
      </dd>
    </div>
  )
}
