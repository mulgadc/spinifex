import { useMemo, useState } from "react"

import {
  type MetricSeries,
  MetricChart,
} from "@/components/charts/metric-chart"
import { PanelSection } from "@/components/detail-panel"
import { Badge } from "@/components/ui/badge"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  formatByteRate,
  formatPacketRate,
  formatPercent,
  METRIC_RANGES,
  type MetricRange,
  RANGE_LABELS,
  simulateInstanceMetrics,
} from "@/lib/simulated-metrics"

interface InstanceMonitoringProps {
  instanceId: string
}

interface ChartSpec {
  title: string
  format: (value: number) => string
  max?: number
  series: MetricSeries[]
}

// EC2's basic metrics, the five that need no agent in the guest. The data is
// simulated until the daemon reports samples from QEMU, and says so.
export function InstanceMonitoring({ instanceId }: InstanceMonitoringProps) {
  const [range, setRange] = useState<MetricRange>("1d")
  // Read on open and on a range change, so the window does not slide under
  // the cursor between renders.
  const [now, setNow] = useState(() => Date.now() / 1000)

  // Built once per range, so the charts are not torn down on every hover.
  const { times, charts } = useMemo(() => {
    const metrics = simulateInstanceMetrics(instanceId, range, now)

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

    return { times: metrics.times, charts: specs }
  }, [instanceId, range, now])

  return (
    <PanelSection span title="Instance statistics">
      <div className="flex flex-col gap-5">
        <div className="flex items-center justify-between gap-2">
          <Badge variant="outline">Simulated data</Badge>
          <ToggleGroup
            aria-label="Time range"
            onValueChange={(values) => {
              const next = METRIC_RANGES.find(
                (value) => value === values.at(-1),
              )

              if (next) {
                setRange(next)
                setNow(Date.now() / 1000)
              }
            }}
            size="sm"
            value={[range]}
            variant="outline"
          >
            {METRIC_RANGES.map((value) => (
              <ToggleGroupItem key={value} value={value}>
                {RANGE_LABELS[value].button}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
        </div>
        <div className="flex flex-col gap-5 in-data-[layout=page]:xl:grid in-data-[layout=page]:xl:grid-cols-3">
          {charts.map((chart) => (
            <MetricChart
              format={chart.format}
              key={chart.title}
              max={chart.max}
              series={chart.series}
              since={RANGE_LABELS[range].since}
              times={times}
              title={chart.title}
            />
          ))}
        </div>
      </div>
    </PanelSection>
  )
}
