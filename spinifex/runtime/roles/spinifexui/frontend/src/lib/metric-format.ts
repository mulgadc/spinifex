// Formatters for the rates the Monitoring charts draw.

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
