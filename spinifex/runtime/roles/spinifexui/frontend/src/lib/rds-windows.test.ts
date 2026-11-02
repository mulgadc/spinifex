import { describe, expect, it } from "vitest"

import {
  formatDailyWindow,
  formatDuration,
  formatWeeklyWindow,
  parseDailyWindow,
  parseWeeklyWindow,
  windowsOverlap,
  withCurrent,
} from "./rds-windows"

describe("daily backup window", () => {
  it("builds AWS's string from a start and a length", () => {
    expect(formatDailyWindow({ start: 3 * 60, duration: 30 })).toBe(
      "03:00-03:30",
    )
  })

  it("ends after midnight when it runs past it", () => {
    expect(formatDailyWindow({ start: 23 * 60 + 30, duration: 60 })).toBe(
      "23:30-00:30",
    )
  })

  it("reads back what it wrote, including a wrapping window", () => {
    expect(parseDailyWindow("23:30-00:30")).toStrictEqual({
      start: 23 * 60 + 30,
      duration: 60,
    })
  })

  it("keeps an off-grid window set elsewhere", () => {
    expect(parseDailyWindow("03:17-04:02")).toStrictEqual({
      start: 3 * 60 + 17,
      duration: 45,
    })
  })

  it.each(["", "3:00-3:30", "24:00-00:30", "03:00", "03:00-03:30-04:00"])(
    "refuses %j",
    (value) => {
      expect(parseDailyWindow(value)).toBeUndefined()
    },
  )
})

describe("weekly maintenance window", () => {
  it("builds AWS's string from a day, a start and a length", () => {
    expect(formatWeeklyWindow({ day: 1, start: 4 * 60, duration: 30 })).toBe(
      "mon:04:00-mon:04:30",
    )
  })

  it("moves to the next day when it runs past midnight", () => {
    expect(
      formatWeeklyWindow({ day: 2, start: 23 * 60 + 30, duration: 60 }),
    ).toBe("tue:23:30-wed:00:30")
  })

  it("wraps from Saturday into Sunday", () => {
    expect(formatWeeklyWindow({ day: 6, start: 23 * 60, duration: 120 })).toBe(
      "sat:23:00-sun:01:00",
    )
  })

  it("reads back a window that wraps the week", () => {
    expect(parseWeeklyWindow("sat:23:00-sun:01:00")).toStrictEqual({
      day: 6,
      start: 23 * 60,
      duration: 120,
    })
  })

  it.each(["", "mon:04:00", "xyz:04:00-xyz:04:30", "mon:4:00-mon:4:30"])(
    "refuses %j",
    (value) => {
      expect(parseWeeklyWindow(value)).toBeUndefined()
    },
  )
})

describe("windowsOverlap", () => {
  it("is false for windows at different times of day", () => {
    expect(windowsOverlap("03:00-03:30", "mon:04:00-mon:04:30")).toBeFalsy()
  })

  it("is false for windows that only touch", () => {
    expect(windowsOverlap("03:00-03:30", "mon:03:30-mon:04:00")).toBeFalsy()
  })

  // The backend projects the maintenance window onto one day, so the day
  // it falls on does not matter.
  it("is true for the same time of day on any weekday", () => {
    expect(windowsOverlap("03:00-04:00", "thu:03:30-thu:04:00")).toBeTruthy()
  })

  it("catches a backup window that wraps midnight", () => {
    expect(windowsOverlap("23:30-00:30", "sun:00:00-sun:00:30")).toBeTruthy()
  })

  it("catches a maintenance window that wraps into the next day", () => {
    expect(windowsOverlap("00:00-00:30", "tue:23:30-wed:00:30")).toBeTruthy()
  })

  it("is false while either window is unset", () => {
    expect(windowsOverlap("", "mon:03:00-mon:03:30")).toBeFalsy()
    expect(windowsOverlap("03:00-03:30", "")).toBeFalsy()
  })
})

describe("formatDuration", () => {
  it.each([
    [30, "30 min"],
    [60, "1 hour"],
    [90, "1 hour 30 min"],
    [480, "8 hours"],
  ])("%i minutes reads %s", (minutes, text) => {
    expect(formatDuration(minutes)).toBe(text)
  })
})

describe("withCurrent", () => {
  it("adds an off-grid current value in order", () => {
    expect(withCurrent([30, 60, 90], 45)).toStrictEqual([30, 45, 60, 90])
  })

  it("leaves the choices alone when the value is already one", () => {
    expect(withCurrent([30, 60], 60)).toStrictEqual([30, 60])
  })
})
