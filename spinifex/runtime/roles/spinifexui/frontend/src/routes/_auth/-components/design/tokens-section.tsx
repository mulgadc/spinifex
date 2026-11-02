import { StateBadge } from "@/components/state-badge"

import { CatalogueSection, Specimen } from "./catalogue-section"

// Class names are written out in full so Tailwind generates them.
const SURFACES = [
  { name: "background", className: "bg-background" },
  { name: "card", className: "bg-card" },
  { name: "popover", className: "bg-popover" },
  { name: "muted", className: "bg-muted" },
  { name: "secondary", className: "bg-secondary" },
  { name: "accent", className: "bg-accent" },
  { name: "primary", className: "bg-primary" },
  { name: "border", className: "bg-border" },
  { name: "sidebar", className: "bg-sidebar" },
]

const STATUSES = [
  {
    name: "success",
    solid: "bg-success",
    tint: "border-success-border bg-success-subtle text-success-foreground",
  },
  {
    name: "warning",
    solid: "bg-warning",
    tint: "border-warning-border bg-warning-subtle text-warning-foreground",
  },
  {
    name: "danger",
    solid: "bg-danger",
    tint: "border-danger-border bg-danger-subtle text-danger-foreground",
  },
  {
    name: "info",
    solid: "bg-info",
    tint: "border-info-border bg-info-subtle text-info-foreground",
  },
  {
    name: "neutral",
    solid: "bg-neutral",
    tint: "border-neutral-border bg-neutral-subtle text-neutral-foreground",
  },
]

const CHARTS = [
  { name: "chart-1", className: "bg-chart-1" },
  { name: "chart-2", className: "bg-chart-2" },
  { name: "chart-3", className: "bg-chart-3" },
  { name: "chart-4", className: "bg-chart-4" },
  { name: "chart-5", className: "bg-chart-5" },
  { name: "chart-6", className: "bg-chart-6" },
  { name: "chart-7", className: "bg-chart-7" },
  { name: "chart-8", className: "bg-chart-8" },
]

const TYPE_SCALE = [
  { name: "2xl · 24 · page title", className: "font-heading text-2xl" },
  { name: "xl · 20 · section", className: "font-heading text-xl" },
  { name: "lg · 16", className: "text-lg" },
  { name: "base · 14", className: "text-base" },
  { name: "sm · 13 · body and tables", className: "text-sm" },
  { name: "xs · 12", className: "text-xs" },
  { name: "2xs · 11", className: "text-2xs" },
]

const SAMPLE_STATES = [
  "running",
  "available",
  "pending",
  "stopping",
  "stopped",
  "failed",
  "terminated",
  "unknown-state",
]

function Swatch({ name, className }: { name: string; className: string }) {
  return (
    <div className="flex w-24 flex-col gap-1.5">
      <div className={`h-12 rounded-md border ${className}`} />
      <span className="font-mono text-2xs text-muted-foreground">{name}</span>
    </div>
  )
}

export function TokensSection() {
  return (
    <CatalogueSection
      description="OKLCH tokens from mulgadc.com. Colour is reserved for status; the orange accent marks the one primary action."
      title="Tokens"
    >
      <Specimen label="Surfaces">
        {SURFACES.map((surface) => (
          <Swatch key={surface.name} {...surface} />
        ))}
      </Specimen>
      <Specimen label="Status: solid, then tinted badge">
        {STATUSES.map((status) => (
          <div className="flex flex-col gap-1.5" key={status.name}>
            <div className={`h-6 w-24 rounded-md ${status.solid}`} />
            <span
              className={`w-fit rounded-full border px-2 py-0.5 text-xs ${status.tint}`}
            >
              {status.name}
            </span>
          </div>
        ))}
      </Specimen>
      <Specimen label="StateBadge: AWS states mapped to tones">
        {SAMPLE_STATES.map((state) => (
          <StateBadge key={state} state={state} />
        ))}
      </Specimen>
      <Specimen label="Chart palette">
        {CHARTS.map((chart) => (
          <Swatch key={chart.name} {...chart} />
        ))}
      </Specimen>
      <Specimen label="Type scale: Archivo headings, Geist text">
        <div className="flex flex-col gap-2">
          {TYPE_SCALE.map((step) => (
            <div
              className="flex flex-wrap items-baseline gap-x-4 gap-y-1"
              key={step.name}
            >
              <span className="w-48 font-mono text-2xs text-muted-foreground">
                {step.name}
              </span>
              <span className={step.className}>Spinifex control plane</span>
            </div>
          ))}
          <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
            <span className="w-48 font-mono text-2xs text-muted-foreground">
              mono · IDs, ARNs, CIDRs
            </span>
            <span className="font-mono text-sm tabular-nums">
              i-0a1b2c3d4e5f67890 · 10.200.0.0/16
            </span>
          </div>
        </div>
      </Specimen>
    </CatalogueSection>
  )
}
