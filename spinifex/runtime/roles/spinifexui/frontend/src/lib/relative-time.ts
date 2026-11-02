const UNITS = [
  ["year", 365 * 24 * 60 * 60],
  ["month", 30 * 24 * 60 * 60],
  ["week", 7 * 24 * 60 * 60],
  ["day", 24 * 60 * 60],
  ["hour", 60 * 60],
  ["min", 60],
] as const

// "33 min ago", "2 days ago", "8 months ago": the largest whole unit, rounded
// down, so a time never reads as later than it was.
export function relativeTime(
  date: Date | string | undefined,
  now: Date = new Date(),
): string {
  if (!date) {
    return "Unknown"
  }

  const seconds = Math.floor((now.getTime() - new Date(date).getTime()) / 1000)

  if (Number.isNaN(seconds)) {
    return "Unknown"
  }

  for (const [unit, size] of UNITS) {
    const count = Math.floor(seconds / size)

    if (count >= 1) {
      const plural = count === 1 || unit === "min" ? unit : `${unit}s`

      return `${count} ${plural} ago`
    }
  }

  return "just now"
}
