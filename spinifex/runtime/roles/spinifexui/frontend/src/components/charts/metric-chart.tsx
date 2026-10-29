import "uplot/dist/uPlot.min.css"
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react"
import uPlot from "uplot"

export const CHART_COLORS = [
  "chart-1",
  "chart-2",
  "chart-3",
  "chart-5",
] as const

export type ChartColor = (typeof CHART_COLORS)[number]

export interface MetricSeries {
  label: string
  color: ChartColor
  // null is a second with no sample, drawn as a gap.
  values: readonly (number | null)[]
}

interface MetricChartProps {
  title: string
  // Unix seconds, one per value, oldest first.
  times: readonly number[]
  series: readonly MetricSeries[]
  format: (value: number) => string
  // Shown at the left of the time axis, e.g. "-24 hrs".
  since: string
  height?: number
  // Pins the y axis, as a percentage does at 100.
  max?: number
}

// The theme class lands on <html> in an effect, after this component's own
// effects run, so the chart watches the class rather than the theme state.
function subscribeToRootClass(onChange: () => void): () => void {
  const observer = new MutationObserver(onChange)

  observer.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ["class"],
  })

  return () => {
    observer.disconnect()
  }
}

function rootClass(): string {
  return document.documentElement.className
}

function token(styles: CSSStyleDeclaration, name: string): string {
  return styles.getPropertyValue(`--${name}`).trim()
}

// oklch(64% 0.21 37) becomes oklch(64% 0.21 37 / 0.3) for the area fill.
function withAlpha(color: string, alpha: number): string {
  return color.replace(/\)$/u, ` / ${alpha})`)
}

function areaFill(color: string) {
  return (u: uPlot) => {
    const gradient = u.ctx.createLinearGradient(
      0,
      u.bbox.top,
      0,
      u.bbox.top + u.bbox.height,
    )

    gradient.addColorStop(0, withAlpha(color, 0.32))
    gradient.addColorStop(1, withAlpha(color, 0))

    return gradient
  }
}

// UniFi draws curves, not polylines.
const smooth = uPlot.paths.spline?.()

function latest(values: readonly (number | null)[]): number | undefined {
  return values.findLast((value) => value !== null) ?? undefined
}

function plotData(
  times: readonly number[],
  series: readonly MetricSeries[],
): uPlot.AlignedData {
  return [[...times], ...series.map((entry) => [...entry.values])]
}

// A small UniFi-style area chart: thin strokes over a fading fill, a muted
// grid, the y axis on the left and the series as chips above the plot.
export function MetricChart({
  title,
  times,
  series,
  format,
  since,
  height = 112,
  max,
}: MetricChartProps) {
  const theme = useSyncExternalStore(subscribeToRootClass, rootClass)
  const [cursor, setCursor] = useState<number>()
  const plotRef = useRef<uPlot>(null)
  // Read when the plot is built; later data arrives through setData, so a
  // live chart is not torn down every second.
  const dataRef = useRef(plotData(times, series))

  const seriesKey = series
    .map((entry) => `${entry.label}:${entry.color}`)
    .join(",")

  useEffect(() => {
    const data = plotData(times, series)

    dataRef.current = data
    plotRef.current?.setData(data)
  }, [times, series])

  // A ref callback with cleanup: the plot lives exactly as long as its element,
  // which the theme key below replaces so the canvas repaints in new colours.
  const attach = useCallback(
    (element: HTMLDivElement) => {
      const styles = getComputedStyle(element)
      const muted = token(styles, "muted-foreground")
      const grid = token(styles, "border")
      const font = `11px ${styles.fontFamily}`

      const plot = new uPlot(
        {
          width: element.clientWidth,
          height,
          padding: [8, 4, 8, 0],
          legend: { show: false },
          cursor: {
            points: { size: 6 },
            x: true,
            y: false,
            drag: { x: false, y: false },
          },
          scales: {
            x: { time: true },
            y: {
              range: (_u, _min, dataMax) => [
                0,
                max ?? Math.max(dataMax * 1.15, 1),
              ],
            },
          },
          axes: [
            { show: false },
            {
              stroke: muted,
              font,
              size: 60,
              gap: 4,
              ticks: { show: false },
              grid: { stroke: grid, width: 1 },
              splits: (_u, _axis, _min, scaleMax) => [
                0,
                scaleMax / 2,
                scaleMax,
              ],
              values: (_u, splits) => splits.map(format),
            },
          ],
          series: [
            {},
            ...seriesKey.split(",").map((key) => {
              const [label = "", colorName = ""] = key.split(":")
              const color = token(styles, colorName)

              return {
                label,
                stroke: color,
                width: 1.5,
                paths: smooth,
                fill: areaFill(color),
                points: { show: false },
              }
            }),
          ],
          hooks: {
            setCursor: [
              (u) => {
                setCursor(u.cursor.idx ?? undefined)
              },
            ],
          },
        },
        dataRef.current,
        element,
      )

      plotRef.current = plot

      const observer = new ResizeObserver(() => {
        plot.setSize({ width: element.clientWidth, height })
      })

      observer.observe(element)

      return () => {
        observer.disconnect()
        plot.destroy()
        plotRef.current = null
      }
    },
    [seriesKey, format, height, max],
  )

  return (
    <figure aria-label={title} className="flex flex-col gap-2">
      <figcaption className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
        <span className="text-xs font-medium">{title}</span>
        <span className="flex flex-wrap gap-1.5">
          {series.map((entry) => {
            const value =
              cursor === undefined
                ? latest(entry.values)
                : (entry.values[cursor] ?? undefined)

            return (
              <span
                className="inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs tabular-nums"
                data-color={entry.color}
                key={entry.label}
              >
                <span
                  aria-hidden
                  className="size-1.5 rounded-full data-[color=chart-1]:bg-chart-1 data-[color=chart-2]:bg-chart-2 data-[color=chart-3]:bg-chart-3 data-[color=chart-5]:bg-chart-5"
                  data-color={entry.color}
                />
                <span className="text-muted-foreground">{entry.label}</span>
                {value === undefined ? "—" : format(value)}
              </span>
            )
          })}
        </span>
      </figcaption>
      <div className="w-full" key={theme} ref={attach} />
      <div className="flex justify-between ps-15 text-2xs text-muted-foreground">
        <span>{since}</span>
        <span>Now</span>
      </div>
    </figure>
  )
}
