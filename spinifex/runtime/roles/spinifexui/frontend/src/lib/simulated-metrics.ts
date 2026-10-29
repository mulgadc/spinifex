// Stand-in for the live EC2 feed until awsgw serves GetMetricData at a
// one-second period. Seeded by instance ID and second, so a given moment
// always draws the same values, which keeps screenshots stable.

// Five minutes of one-second samples: what the collector's ring holds.
export const LIVE_WINDOW_SECONDS = 300

export interface InstanceMetrics {
  times: number[]
  cpu: (number | null)[]
  networkIn: (number | null)[]
  networkOut: (number | null)[]
  packetsIn: (number | null)[]
  packetsOut: (number | null)[]
}

export interface LiveSample {
  cpu: number
  networkIn: number
  networkOut: number
  packetsIn: number
  packetsOut: number
}

const MODULUS = 2_147_483_647

function seedOf(text: string): number {
  let value = 7

  for (const char of text) {
    value = (value * 31 + (char.codePointAt(0) ?? 0)) % MODULUS
  }

  return value || 1
}

// Park-Miller: small, exact in doubles, and plenty for plausible noise. A few
// draws are thrown away so seeds a step apart do not start alike.
function randomAt(seed: number, index: number): number {
  let state = Math.abs(seed + index * 7919) % MODULUS || 1

  for (let warm = 0; warm < 4; warm += 1) {
    state = (state * 48_271) % MODULUS
  }

  return state / MODULUS
}

// Value noise: random knots every few seconds, eased between, so the line
// wanders the way real load does instead of jumping every sample.
function smoothNoise(seed: number, time: number, period: number): number {
  const knot = Math.floor(time / period)
  const fraction = (time - knot * period) / period
  const eased = (1 - Math.cos(fraction * Math.PI)) / 2

  return randomAt(seed, knot) * (1 - eased) + randomAt(seed, knot + 1) * eased
}

export function simulateLiveSample(
  instanceId: string,
  time: number,
): LiveSample {
  const seed = seedOf(instanceId)
  const baseline = 4 + randomAt(seed, -1) * 26
  const swell = smoothNoise(seed, time, 30)
  const burst = smoothNoise(seed + 1, time, 8) > 0.85 ? 40 : 0
  const jitter = smoothNoise(seed + 2, time, 4)
  const cpu = Math.min(100, baseline * (0.6 + swell) + burst + jitter * 4)
  const load = cpu / 100
  const bytesIn = (40_000 + load * 2_400_000) * (0.7 + jitter * 0.6)
  const bytesOut = (25_000 + load * 1_100_000) * (0.7 + (1 - jitter) * 0.6)

  return {
    cpu,
    networkIn: bytesIn,
    networkOut: bytesOut,
    packetsIn: bytesIn / (600 + jitter * 300),
    packetsOut: bytesOut / (400 + jitter * 300),
  }
}

// The window ending at `now`, with samples only from `since` on: the ring
// starts when someone starts watching, so earlier seconds are gaps.
export function liveWindow(
  instanceId: string,
  since: number,
  now: number,
): InstanceMetrics {
  const metrics: InstanceMetrics = {
    times: [],
    cpu: [],
    networkIn: [],
    networkOut: [],
    packetsIn: [],
    packetsOut: [],
  }

  for (let time = now - LIVE_WINDOW_SECONDS + 1; time <= now; time += 1) {
    const sample = time >= since ? simulateLiveSample(instanceId, time) : null

    metrics.times.push(time)
    metrics.cpu.push(sample?.cpu ?? null)
    metrics.networkIn.push(sample?.networkIn ?? null)
    metrics.networkOut.push(sample?.networkOut ?? null)
    metrics.packetsIn.push(sample?.packetsIn ?? null)
    metrics.packetsOut.push(sample?.packetsOut ?? null)
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
