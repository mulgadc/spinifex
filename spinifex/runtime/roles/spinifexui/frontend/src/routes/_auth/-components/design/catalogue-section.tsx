import type { ReactNode } from "react"

export function CatalogueSection({
  title,
  description,
  children,
}: {
  title: string
  description?: string
  children: ReactNode
}) {
  return (
    <section className="flex flex-col gap-4">
      <div>
        <h2 className="text-xl font-semibold">{title}</h2>
        {description && (
          <p className="mt-1 text-muted-foreground">{description}</p>
        )}
      </div>
      {children}
    </section>
  )
}

export function Specimen({
  label,
  children,
}: {
  label: string
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-3 rounded-lg border bg-card p-4">
      <span className="font-mono text-2xs tracking-wider text-muted-foreground uppercase">
        {label}
      </span>
      <div className="flex flex-wrap items-center gap-3">{children}</div>
    </div>
  )
}

// For a specimen's buttons that have nothing to act on.
export function unwired() {
  // A specimen shows the control, not its effect.
}
