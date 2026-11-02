import {
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react"

import { type LiveAxis, MetricChart } from "@/components/charts/metric-chart"
import { PanelSection } from "@/components/detail-panel"
import { LiveClock, type LiveStatus } from "@/lib/live-metrics"
import { LIVE_WINDOW_MS } from "@/lib/live-sample"
import {
  formatByteRate,
  formatPacketRate,
  formatPercent,
} from "@/lib/metric-format"
import {
  NodeLiveBuffer,
  type NodeLiveSeries,
  watchNodeMetrics,
} from "@/lib/node-live"
import { planesLabel } from "@/lib/node-topology"

const STATUS_TEXT: Record<Exclude<LiveStatus, "live">, string> = {
  connecting: "Connecting…",
  unavailable: "This node is not answering for live statistics. Retrying.",
  unsupported: "This cluster's gateway does not serve node statistics.",
  error: "Live statistics are unavailable. Retrying.",
}

function subscribeToVisibility(onChange: () => void): () => void {
  document.addEventListener("visibilitychange", onChange)

  return () => {
    document.removeEventListener("visibilitychange", onChange)
  }
}

function pageVisible(): boolean {
  return document.visibilityState === "visible"
}

const EMPTY = new NodeLiveBuffer().series()

interface View {
  node: string
  series: NodeLiveSeries
  status: LiveStatus
}

// Streams while the tab is visible; a hidden tab ends the stream, so the node
// stops sampling itself once its lease lapses.
function useNodeMetrics(node: string) {
  const visible = useSyncExternalStore(subscribeToVisibility, pageVisible)
  const clock = useRef(new LiveClock())

  const [view, setView] = useState<View>({
    node,
    series: EMPTY,
    status: "connecting",
  })

  useEffect(() => {
    const controller = new AbortController()

    if (visible) {
      const buffer = new NodeLiveBuffer()
      const streamClock = new LiveClock()

      clock.current = streamClock
      void watchNodeMetrics(
        node,
        0,
        {
          onSamples: (samples) => {
            const arrival = Date.now()

            for (const sample of samples) {
              streamClock.observe(sample.t, arrival)
            }

            buffer.add(samples)
            const series = buffer.series()
            setView((previous) => ({ ...previous, node, series }))
          },
          onStatus: (status) => {
            setView((previous) => ({ ...previous, node, status }))
          },
        },
        controller.signal,
      )
    }

    return () => {
      controller.abort()
    }
  }, [node, visible])

  const live = useMemo<LiveAxis>(
    () => ({
      now: () => clock.current.now(Date.now()) / 1000,
      span: LIVE_WINDOW_MS / 1000,
    }),
    [],
  )

  const current = view.node === node

  return {
    series: current ? view.series : EMPTY,
    status: current ? view.status : ("connecting" as const),
    live,
  }
}

export function NodeMetrics({ node }: { node: string }) {
  const { series, status, live } = useNodeMetrics(node)

  return (
    <PanelSection title="Host statistics">
      <div className="flex flex-col gap-5">
        <div className="flex items-center gap-2">
          {status === "live" ? (
            <span className="inline-flex items-center gap-1.5 text-xs font-medium">
              <span
                aria-hidden
                className="size-2 animate-pulse rounded-full bg-success motion-reduce:animate-none"
              />
              Live
            </span>
          ) : (
            <output className="text-xs text-muted-foreground">
              {STATUS_TEXT[status]}
            </output>
          )}
        </div>
        <MetricChart
          format={formatPercent}
          live={live}
          max={100}
          series={[
            { label: "CPU", color: "chart-1", values: series.cpu.cpu },
            { label: "Memory", color: "chart-3", values: series.memory.cpu },
          ]}
          since="-5 min"
          times={series.cpu.times}
          title="CPU and memory (%)"
        />
        {series.planes.flatMap(([plane, planeSeries]) => [
          <MetricChart
            format={formatByteRate}
            key={`${plane}-bytes`}
            live={live}
            series={[
              { label: "In", color: "chart-2", values: planeSeries.networkIn },
              {
                label: "Out",
                color: "chart-3",
                values: planeSeries.networkOut,
              },
            ]}
            since="-5 min"
            times={planeSeries.times}
            title={`${planesLabel(plane.split("+"))} network`}
          />,
          <MetricChart
            format={formatPacketRate}
            key={`${plane}-packets`}
            live={live}
            series={[
              { label: "In", color: "chart-2", values: planeSeries.packetsIn },
              {
                label: "Out",
                color: "chart-5",
                values: planeSeries.packetsOut,
              },
            ]}
            since="-5 min"
            times={planeSeries.times}
            title={`${planesLabel(plane.split("+"))} packets`}
          />,
        ])}
      </div>
    </PanelSection>
  )
}
