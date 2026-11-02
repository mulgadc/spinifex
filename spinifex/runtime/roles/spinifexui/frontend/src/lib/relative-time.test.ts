import { describe, expect, it } from "vitest"

import { relativeTime } from "./relative-time"

const NOW = new Date("2026-11-01T12:00:00Z")

function ago(seconds: number): Date {
  return new Date(NOW.getTime() - seconds * 1000)
}

describe("relativeTime", () => {
  it.each([
    [30, "just now"],
    [33 * 60, "33 min ago"],
    [60 * 60, "1 hour ago"],
    [2 * 24 * 60 * 60 + 5, "2 days ago"],
    [7 * 24 * 60 * 60, "1 week ago"],
    [240 * 24 * 60 * 60, "8 months ago"],
    [400 * 24 * 60 * 60, "1 year ago"],
  ])("%i seconds back reads %s", (seconds, expected) => {
    expect(relativeTime(ago(seconds), NOW)).toBe(expected)
  })

  it("rounds down, so 59 minutes is never an hour", () => {
    expect(relativeTime(ago(59 * 60 + 59), NOW)).toBe("59 min ago")
  })

  it("reads an absent or unparseable time as unknown", () => {
    expect(relativeTime(undefined, NOW)).toBe("Unknown")
    expect(relativeTime("not a date", NOW)).toBe("Unknown")
  })
})
