// Transfer rates are given in bits, as network links are, with SI prefixes.
const BIT_UNITS = ["bit/s", "kbit/s", "Mbit/s", "Gbit/s", "Tbit/s"] as const

export function formatBitRate(bytesPerSecond: number): string {
  let value = bytesPerSecond * 8
  let unit = 0

  while (value >= 1000 && unit < BIT_UNITS.length - 1) {
    value /= 1000
    unit += 1
  }

  const digits = value < 10 && unit > 0 ? 1 : 0

  return `${value.toFixed(digits)} ${BIT_UNITS[unit]}`
}

// "about 3 min left" rather than a ticking clock, which jitters with the rate.
export function formatRemaining(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) {
    return ""
  }

  if (seconds < 5) {
    return "a few seconds left"
  }

  if (seconds < 60) {
    return `${Math.ceil(seconds / 5) * 5} s left`
  }

  if (seconds < 3600) {
    return `about ${Math.round(seconds / 60)} min left`
  }

  const hours = Math.floor(seconds / 3600)
  const minutes = Math.round((seconds % 3600) / 60)

  return `about ${hours} h ${minutes} min left`
}

export function formatElapsed(milliseconds: number): string {
  const seconds = Math.max(0, Math.round(milliseconds / 1000))

  if (seconds < 60) {
    return `${seconds} s`
  }

  const minutes = Math.floor(seconds / 60)

  return `${minutes} min ${seconds % 60} s`
}

export interface RateSample {
  // Milliseconds, from performance.now().
  at: number
  loaded: number
}

// Over the last few seconds rather than since the start, so the figure follows
// a link that speeds up or stalls instead of averaging it away.
export const RATE_WINDOW_MS = 3000

export function currentRate(samples: readonly RateSample[]): number {
  const last = samples.at(-1)

  if (!last) {
    return 0
  }

  const first =
    samples.find((sample) => last.at - sample.at <= RATE_WINDOW_MS) ?? last

  const elapsed = (last.at - first.at) / 1000

  return elapsed > 0.25 ? (last.loaded - first.loaded) / elapsed : 0
}

// Whole percent, floored so 100% means every byte has gone.
export function percentOf(loaded: number, total: number): number {
  return total > 0 ? Math.floor((loaded / total) * 100) : 0
}
