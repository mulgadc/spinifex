import { useEffect, useMemo, useState } from "react"

import {
  type MetricSeries,
  MetricChart,
} from "@/components/charts/metric-chart"
import { PanelSection } from "@/components/detail-panel"
import { Badge } from "@/components/ui/badge"
import {
  formatByteRate,
  formatPacketRate,
  formatPercent,
  liveWindow,
} from "@/lib/simulated-metrics"

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

function nowSeconds(): number {
  return Math.floor(Date.now() / 1000)
}

// Ticks once a second while the page is visible, as the console will poll
// GetMetricData; a hidden tab stops asking, so the host stops sampling.
function useSecondTick(enabled: boolean): number {
  const [now, setNow] = useState(nowSeconds)

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (enabled && document.visibilityState === "visible") {
        setNow(nowSeconds())
      }
    }, 1000)

    return () => {
      window.clearInterval(timer)
    }
  }, [enabled])

  return now
}

// EC2's agentless metrics, live at one second over the last five minutes, as
// Unikraft shows them. There is no history to range over yet, and the data is
// simulated until awsgw serves it, which the badge says.
export function InstanceMonitoring({
  instanceId,
  running,
}: InstanceMonitoringProps) {
  // When this viewer started watching: the ring holds nothing earlier.
  const since = useMemo(() => nowSeconds(), [])
  const now = useSecondTick(running)

  const charts = useMemo(() => {
    const metrics = liveWindow(instanceId, running ? since : now + 1, now)

    const specs: ChartSpec[] = [
      {
        title: "CPU utilization (%)",
        format: formatPercent,
        max: 100,
        series: [{ label: "CPU", color: "chart-1", values: metrics.cpu }],
      },
      {
        title: "Network",
        format: formatByteRate,
        series: [
          { label: "In", color: "chart-2", values: metrics.networkIn },
          { label: "Out", color: "chart-3", values: metrics.networkOut },
        ],
      },
      {
        title: "Network packets",
        format: formatPacketRate,
        series: [
          { label: "In", color: "chart-2", values: metrics.packetsIn },
          { label: "Out", color: "chart-5", values: metrics.packetsOut },
        ],
      },
    ]

    return { times: metrics.times, specs }
  }, [instanceId, running, since, now])

  return (
    <PanelSection title="Instance statistics">
      <div className="flex flex-col gap-5">
        <div className="flex items-center justify-between gap-2">
          {running ? (
            <span className="inline-flex items-center gap-1.5 text-xs font-medium">
              <span
                aria-hidden
                className="size-2 animate-pulse rounded-full bg-success motion-reduce:animate-none"
              />
              Live
            </span>
          ) : (
            <span className="text-xs text-muted-foreground">
              Live statistics need a running instance.
            </span>
          )}
          <Badge variant="outline">Simulated data</Badge>
        </div>
        {charts.specs.map((chart) => (
          <MetricChart
            format={chart.format}
            key={chart.title}
            max={chart.max}
            series={chart.series}
            since="-5 min"
            times={charts.times}
            title={chart.title}
          />
        ))}
      </div>
    </PanelSection>
  )
}
