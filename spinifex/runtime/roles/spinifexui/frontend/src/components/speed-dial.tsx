import {
  formatBitRate,
  formatLinkSpeed,
  rateThroughput,
  type ThroughputRating,
} from "@/lib/node-topology"
import { cn } from "@/lib/utils"

const RATING_TONE: Record<ThroughputRating, string> = {
  excellent: "text-success",
  good: "text-success",
  fair: "text-warning",
  poor: "text-danger",
  unknown: "text-muted-foreground",
}

const RATING_LABEL: Record<ThroughputRating, string> = {
  excellent: "Excellent",
  good: "Good",
  fair: "Fair",
  poor: "Poor",
  unknown: "Link speed unknown",
}

interface SpeedDialProps {
  bitsPerSecond: number
  // The port's line rate, which is the dial's full scale.
  linkMbps?: number
  label?: string
  className?: string
}

const RADIUS = 80
const CENTRE = { x: 100, y: 96 }

function point(fraction: number, radius = RADIUS) {
  const angle = Math.PI * (1 - fraction)

  return {
    x: CENTRE.x + radius * Math.cos(angle),
    y: CENTRE.y - radius * Math.sin(angle),
  }
}

function arc(from: number, to: number) {
  const start = point(from)
  const end = point(to)

  return `M ${start.x} ${start.y} A ${RADIUS} ${RADIUS} 0 0 1 ${end.x} ${end.y}`
}

// A half-circle gauge whose full scale is the link's own speed, so the needle
// reads as a share of what the port can carry.
export function SpeedDial({
  bitsPerSecond,
  linkMbps,
  label,
  className,
}: SpeedDialProps) {
  const { percent, rating } = rateThroughput(bitsPerSecond, linkMbps)
  const fraction = percent / 100
  const needle = point(fraction, RADIUS - 18)
  const ticks = [0, 0.25, 0.5, 0.75, 1]

  return (
    <figure className={cn("flex flex-col items-center gap-1", className)}>
      <svg aria-hidden className="w-full max-w-56" viewBox="0 0 200 112">
        <path
          className="stroke-muted"
          d={arc(0, 1)}
          fill="none"
          strokeLinecap="round"
          strokeWidth={12}
        />
        {fraction > 0 && (
          <path
            className={cn("stroke-current", RATING_TONE[rating])}
            d={arc(0, Math.max(fraction, 0.005))}
            fill="none"
            strokeLinecap="round"
            strokeWidth={12}
          />
        )}
        {ticks.map((tick) => {
          const inner = point(tick, RADIUS - 10)
          const outer = point(tick, RADIUS + 10)

          return (
            <line
              className="stroke-muted-foreground"
              key={tick}
              strokeWidth={1}
              x1={inner.x}
              x2={outer.x}
              y1={inner.y}
              y2={outer.y}
            />
          )
        })}
        <line
          className="stroke-foreground"
          strokeLinecap="round"
          strokeWidth={2.5}
          x1={CENTRE.x}
          x2={needle.x}
          y1={CENTRE.y}
          y2={needle.y}
        />
        <circle className="fill-foreground" cx={CENTRE.x} cy={CENTRE.y} r={4} />
        <text
          className="fill-muted-foreground text-2xs"
          textAnchor="start"
          x={14}
          y={110}
        >
          0
        </text>
        <text
          className="fill-muted-foreground text-2xs"
          textAnchor="end"
          x={186}
          y={110}
        >
          {linkMbps ? formatLinkSpeed(linkMbps) : "?"}
        </text>
      </svg>
      <figcaption className="flex flex-col items-center">
        <span className="font-mono text-lg font-semibold tabular-nums">
          {formatBitRate(bitsPerSecond)}
        </span>
        <span className={cn("text-xs font-medium", RATING_TONE[rating])}>
          {RATING_LABEL[rating]}
          {rating !== "unknown" && ` · ${Math.round(percent)}% of line rate`}
        </span>
        {label && (
          <span className="text-2xs text-muted-foreground">{label}</span>
        )}
      </figcaption>
    </figure>
  )
}
