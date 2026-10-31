import {
  type Dispatch,
  type SetStateAction,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react"

import {
  type LiveAxis,
  type MetricSeries,
  MetricChart,
} from "@/components/charts/metric-chart"
import { PanelSection } from "@/components/detail-panel"
import { LiveBuffer, type LiveSeries } from "@/lib/live-buffer"
import {
  LiveClock,
  type LiveStatus,
  watchInstanceMetrics,
} from "@/lib/live-metrics"
import { LIVE_WINDOW_MS } from "@/lib/live-sample"
import {
  formatByteRate,
  formatPacketRate,
  formatPercent,
} from "@/lib/metric-format"

interface InstanceMonitoringProps {
  instanceId: string
  running: boolean
}

interface ChartSpec {
  title: string
  format: (value: number) => string
  max?: number
  series: MetricSeries[]
}

const STATUS_TEXT: Record<Exclude<LiveStatus, "live">, string> = {
  connecting: "Connecting…",
  unavailable: "No live statistics for this instance yet. Retrying.",
  unsupported: "This cluster's gateway does not serve live statistics.",
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

interface LiveView {
  instanceId: string
  series: LiveSeries
  status: LiveStatus
}

const EMPTY_SERIES = new LiveBuffer().series()

// Runs one stream into a fresh buffer and clock, since the host replays its
// whole window on connect.
function streamLiveView(
  instanceId: string,
  clock: { current: LiveClock },
  setView: Dispatch<SetStateAction<LiveView>>,
  signal: AbortSignal,
) {
  const buffer = new LiveBuffer()
  const streamClock = new LiveClock()

  clock.current = streamClock

  void watchInstanceMetrics(
    instanceId,
    0,
    {
      onSamples: (samples) => {
        const arrival = Date.now()

        for (const sample of samples) {
          streamClock.observe(sample.t, arrival)
        }

        buffer.add(samples)

        const series = buffer.series()
        setView((previous) => ({ ...previous, instanceId, series }))
      },
      onStatus: (status) => {
        setView((previous) => ({ ...previous, instanceId, status }))
      },
    },
    signal,
  )
}

// Streams the instance's statistics while it runs and the page is visible; a
// hidden tab ends the stream, so the host stops sampling for it.
function useLiveMetrics(instanceId: string, running: boolean) {
  const visible = useSyncExternalStore(subscribeToVisibility, pageVisible)
  const clock = useRef(new LiveClock())

  const [view, setView] = useState<LiveView>({
    instanceId,
    series: EMPTY_SERIES,
    status: "connecting",
  })

  useEffect(() => {
    const controller = new AbortController()

    if (running && visible) {
      streamLiveView(instanceId, clock, setView, controller.signal)
    }

    return () => {
      controller.abort()
    }
  }, [instanceId, running, visible])

  const live = useMemo<LiveAxis>(
    () => ({
      now: () => clock.current.now(Date.now()) / 1000,
      span: LIVE_WINDOW_MS / 1000,
    }),
    [],
  )

  // Until the new instance's stream reports, show nothing of the last one's.
  const current = view.instanceId === instanceId

  return {
    series: current ? view.series : EMPTY_SERIES,
    status: current ? view.status : ("connecting" as const),
    live,
  }
}

// EC2's agentless metrics, streamed from the host every 500 ms over the last
// five minutes, as Unikraft shows them. There is no history to range over.
export function InstanceMonitoring({
  instanceId,
  running,
}: InstanceMonitoringProps) {
  const { series, status, live } = useLiveMetrics(instanceId, running)

  const specs = useMemo<ChartSpec[]>(
    () => [
      {
        title: "CPU utilization (%)",
        format: formatPercent,
        max: 100,
        series: [{ label: "CPU", color: "chart-1", values: series.cpu }],
      },
      {
        title: "Network",
        format: formatByteRate,
        series: [
          { label: "In", color: "chart-2", values: series.networkIn },
          { label: "Out", color: "chart-3", values: series.networkOut },
        ],
      },
      {
        title: "Network packets",
        format: formatPacketRate,
        series: [
          { label: "In", color: "chart-2", values: series.packetsIn },
          { label: "Out", color: "chart-5", values: series.packetsOut },
        ],
      },
    ],
    [series],
  )

  let state = (
    <span className="text-xs text-muted-foreground">
      Live statistics need a running instance.
    </span>
  )

  if (running) {
    state =
      status === "live" ? (
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
      )
  }

  return (
    <PanelSection title="Instance statistics">
      <div className="flex flex-col gap-5">
        <div className="flex items-center gap-2">{state}</div>
        {specs.map((chart) => (
          <MetricChart
            format={chart.format}
            key={chart.title}
            live={live}
            max={chart.max}
            series={chart.series}
            since="-5 min"
            times={series.times}
            title={chart.title}
          />
        ))}
      </div>
    </PanelSection>
  )
}
