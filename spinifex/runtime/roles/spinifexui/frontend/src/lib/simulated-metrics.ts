// Stand-in for EC2 basic monitoring until the daemon serves samples from QEMU.
// Seeded by instance ID, so a chart is stable across renders and reloads.

export const METRIC_RANGES = ["1h", "1d", "1w"] as const

export type MetricRange = (typeof METRIC_RANGES)[number]

export const RANGE_LABELS = {
  "1h": { button: "1H", since: "-1 hr" },
  "1d": { button: "1D", since: "-24 hrs" },
  "1w": { button: "1W", since: "-7 days" },
} satisfies Record<MetricRange, { button: string; since: string }>

// Basic monitoring's five-minute period, finer for the hour and coarser for the
// week so each range draws a similar number of points.
const STEP_SECONDS = {
  "1h": 60,
  "1d": 300,
  "1w": 3600,
} satisfies Record<MetricRange, number>

const RANGE_SECONDS = {
  "1h": 3600,
  "1d": 86_400,
  "1w": 604_800,
} satisfies Record<MetricRange, number>

export interface InstanceMetrics {
  times: number[]
  cpu: number[]
  networkIn: number[]
  networkOut: number[]
  packetsIn: number[]
  packetsOut: number[]
}

const MODULUS = 2_147_483_647

function seedOf(text: string): number {
  let value = 7

  for (const char of text) {
    value = (value * 31 + (char.codePointAt(0) ?? 0)) % MODULUS
  }

  return value || 1
}

// Park-Miller: small, exact in doubles, and plenty for plausible noise.
// IDs that differ in one character start a step apart, so the first few
// draws are thrown away to set their series apart.
function generator(seed: number): () => number {
  let state = seed

  function draw(): number {
    state = (state * 48_271) % MODULUS

    return state / MODULUS
  }

  for (let warm = 0; warm < 4; warm += 1) {
    draw()
  }

  return draw
}

export function simulateInstanceMetrics(
  instanceId: string,
  range: MetricRange,
  now: number,
): InstanceMetrics {
  const rand = generator(seedOf(instanceId))
  const step = STEP_SECONDS[range]
  const end = Math.floor(now / step) * step
  const count = RANGE_SECONDS[range] / step
  const baseline = 4 + rand() * 26

  const metrics: InstanceMetrics = {
    times: [],
    cpu: [],
    networkIn: [],
    networkOut: [],
    packetsIn: [],
    packetsOut: [],
  }

  let burst = 0

  for (let index = 0; index <= count; index += 1) {
    const time = end - (count - index) * step
    // A working-day swell, plus the odd burst that decays over a few samples.
    const daily = Math.sin(((time % 86_400) / 86_400) * Math.PI * 2 - 2) + 1
    burst = rand() > 0.97 ? 30 + rand() * 50 : burst * 0.6

    const cpu = Math.min(
      100,
      baseline * (0.6 + daily * 0.5) + burst + rand() * 4,
    )

    const load = cpu / 100

    const bytesIn = (40_000 + load * 2_400_000) * (0.7 + rand() * 0.6)
    const bytesOut = (25_000 + load * 1_100_000) * (0.7 + rand() * 0.6)

    metrics.times.push(time)
    metrics.cpu.push(cpu)
    metrics.networkIn.push(bytesIn)
    metrics.networkOut.push(bytesOut)
    metrics.packetsIn.push(bytesIn / (600 + rand() * 300))
    metrics.packetsOut.push(bytesOut / (400 + rand() * 300))
  }

  return metrics
}

const BYTE_UNITS = ["B/s", "KB/s", "MB/s", "GB/s"]

export function formatByteRate(value: number): string {
  const exponent = Math.min(
    BYTE_UNITS.length - 1,
    Math.max(0, Math.floor(Math.log10(Math.max(value, 1)) / 3)),
  )

  const scaled = value / 1000 ** exponent

  return `${scaled.toFixed(scaled < 10 && exponent > 0 ? 1 : 0)} ${BYTE_UNITS[exponent]}`
}

export function formatPacketRate(value: number): string {
  return value >= 1000
    ? `${(value / 1000).toFixed(1)}k/s`
    : `${Math.round(value)}/s`
}

export function formatPercent(value: number): string {
  return `${Math.round(value)}%`
}
