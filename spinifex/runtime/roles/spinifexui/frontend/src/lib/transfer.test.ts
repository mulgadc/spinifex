import { describe, expect, it } from "vitest"

import {
  currentRate,
  formatBitRate,
  formatElapsed,
  formatRemaining,
  percentOf,
} from "./transfer"

describe("formatBitRate", () => {
  it.each([
    [0, "0 bit/s"],
    [100, "800 bit/s"],
    [125, "1.0 kbit/s"],
    [1250, "10 kbit/s"],
    [1_250_000, "10 Mbit/s"],
    [312_500, "2.5 Mbit/s"],
    [125_000_000, "1.0 Gbit/s"],
    [3_125_000_000, "25 Gbit/s"],
    [125e12, "1000 Tbit/s"],
  ])("gives %d bytes a second as %s", (bytes, label) => {
    expect(formatBitRate(bytes)).toBe(label)
  })
})

describe("formatRemaining", () => {
  it.each([
    [Number.NaN, ""],
    [Number.POSITIVE_INFINITY, ""],
    [-1, ""],
    [3, "a few seconds left"],
    [12, "15 s left"],
    [90, "about 2 min left"],
    [3 * 3600 + 20 * 60, "about 3 h 20 min left"],
  ])("gives %d seconds as %j", (seconds, label) => {
    expect(formatRemaining(seconds)).toBe(label)
  })
})

describe("formatElapsed", () => {
  it("gives seconds, then minutes and seconds", () => {
    expect(formatElapsed(4400)).toBe("4 s")
    expect(formatElapsed(125_000)).toBe("2 min 5 s")
  })
})

describe("currentRate", () => {
  it("is zero with too little to measure", () => {
    expect(currentRate([])).toBe(0)
    expect(currentRate([{ at: 0, loaded: 0 }])).toBe(0)
    expect(
      currentRate([
        { at: 0, loaded: 0 },
        { at: 100, loaded: 1000 },
      ]),
    ).toBe(0)
  })

  it("measures over the last three seconds only", () => {
    // A fast first second, then a steady 1,000 bytes a second.
    const samples = [
      { at: 0, loaded: 0 },
      { at: 1000, loaded: 50_000 },
      { at: 2000, loaded: 51_000 },
      { at: 3000, loaded: 52_000 },
      { at: 4000, loaded: 53_000 },
    ]

    expect(currentRate(samples)).toBe(1000)
  })
})

describe("percentOf", () => {
  it("floors, so 100% means every byte", () => {
    expect(percentOf(999, 1000)).toBe(99)
    expect(percentOf(1000, 1000)).toBe(100)
    expect(percentOf(0, 0)).toBe(0)
  })
})
