// AWS's backup and maintenance windows, both in UTC: "hh24:mi-hh24:mi" for the
// daily backup window and "ddd:hh24:mi-ddd:hh24:mi" for the weekly maintenance
// window. The form edits a start and a length; these turn one into the other.

export const MINUTES_PER_DAY = 24 * 60
const MINUTES_PER_WEEK = 7 * MINUTES_PER_DAY

export const WEEKDAYS = [
  "sun",
  "mon",
  "tue",
  "wed",
  "thu",
  "fri",
  "sat",
] as const

export const WEEKDAY_LABELS: Record<(typeof WEEKDAYS)[number], string> = {
  sun: "Sunday",
  mon: "Monday",
  tue: "Tuesday",
  wed: "Wednesday",
  thu: "Thursday",
  fri: "Friday",
  sat: "Saturday",
}

// Every half hour, which is AWS's granularity for an assigned window.
export const START_TIMES: readonly number[] = Array.from(
  { length: MINUTES_PER_DAY / 30 },
  (_, index) => index * 30,
)

export const DURATIONS: readonly number[] = [
  30, 60, 90, 120, 180, 240, 360, 480,
]

export interface DailyWindow {
  start: number
  duration: number
}

export interface WeeklyWindow extends DailyWindow {
  day: number
}

const CLOCK = /^(?<h>[01]\d|2[0-3]):(?<m>[0-5]\d)$/u

function parseClock(value: string): number | undefined {
  const match = CLOCK.exec(value)

  if (!match?.groups) {
    return undefined
  }

  return Number(match.groups.h) * 60 + Number(match.groups.m)
}

export function formatClock(minutes: number): string {
  const inDay =
    ((minutes % MINUTES_PER_DAY) + MINUTES_PER_DAY) % MINUTES_PER_DAY

  const hours = String(Math.floor(inDay / 60)).padStart(2, "0")
  const mins = String(inDay % 60).padStart(2, "0")

  return `${hours}:${mins}`
}

function forward(start: number, end: number, period: number): number {
  return (((end - start) % period) + period) % period
}

export function parseDailyWindow(value: string): DailyWindow | undefined {
  const [from = "", to = "", extra] = value.split("-")
  const start = parseClock(from)
  const end = parseClock(to)

  if (extra !== undefined || start === undefined || end === undefined) {
    return undefined
  }

  return { start, duration: forward(start, end, MINUTES_PER_DAY) }
}

export function formatDailyWindow({ start, duration }: DailyWindow): string {
  return `${formatClock(start)}-${formatClock(start + duration)}`
}

function parseWeekdayClock(value: string): number | undefined {
  const day = WEEKDAYS.findIndex((name) => value.startsWith(`${name}:`))
  const clock = parseClock(value.slice(4))

  if (day === -1 || clock === undefined) {
    return undefined
  }

  return day * MINUTES_PER_DAY + clock
}

export function parseWeeklyWindow(value: string): WeeklyWindow | undefined {
  const [from = "", to = "", extra] = value.split("-")
  const start = parseWeekdayClock(from)
  const end = parseWeekdayClock(to)

  if (extra !== undefined || start === undefined || end === undefined) {
    return undefined
  }

  return {
    day: Math.floor(start / MINUTES_PER_DAY),
    start: start % MINUTES_PER_DAY,
    duration: forward(start, end, MINUTES_PER_WEEK),
  }
}

// A window running past midnight ends on the next day, and past Saturday on
// Sunday, as AWS writes it.
export function formatWeeklyWindow({
  day,
  start,
  duration,
}: WeeklyWindow): string {
  const from = day * MINUTES_PER_DAY + start
  const to = (from + duration) % MINUTES_PER_WEEK

  const stamp = (minutes: number) =>
    `${WEEKDAYS[Math.floor(minutes / MINUTES_PER_DAY)]}:${formatClock(minutes)}`

  return `${stamp(from)}-${stamp(to)}`
}

// A window as the non-wrapping spans of one period it covers.
function segments(
  start: number,
  duration: number,
  period: number,
): [number, number][] {
  if (duration >= period) {
    return [[0, period]]
  }

  const end = start + duration

  return end <= period
    ? [[start, end]]
    : [
        [start, period],
        [0, end - period],
      ]
}

// The backend's rule: the maintenance window is projected onto a single day,
// so the pair is refused whatever day the maintenance falls on.
export function windowsOverlap(backup: string, maintenance: string): boolean {
  const daily = parseDailyWindow(backup)
  const weekly = parseWeeklyWindow(maintenance)

  if (!daily || !weekly) {
    return false
  }

  const a = segments(daily.start, daily.duration, MINUTES_PER_DAY)
  const b = segments(weekly.start, weekly.duration, MINUTES_PER_DAY)

  return a.some(([aStart, aEnd]) =>
    b.some(([bStart, bEnd]) => aStart < bEnd && bStart < aEnd),
  )
}

export function formatDuration(minutes: number): string {
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60

  if (hours === 0) {
    return `${rest} min`
  }

  const hourText = `${hours} hour${hours === 1 ? "" : "s"}`

  return rest === 0 ? hourText : `${hourText} ${rest} min`
}

// The standard choices plus the current one, so a window set elsewhere at an
// odd time or length is shown as it is rather than silently moved.
export function withCurrent(
  choices: readonly number[],
  current: number | undefined,
): number[] {
  if (current === undefined || choices.includes(current)) {
    return [...choices]
  }

  return [...choices, current].toSorted((a, b) => a - b)
}
