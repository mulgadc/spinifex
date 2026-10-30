import { describe, expect, it } from "vitest"

import { LiveBuffer } from "./live-buffer"
import { LIVE_WINDOW_MS, type LiveSample } from "./live-sample"

function at(t: number, cpu = 1): LiveSample {
  return { t, cpu, netIn: 0, netOut: 0, pktIn: 0, pktOut: 0 }
}

describe("LiveBuffer", () => {
  it("keeps each sample once, in order", () => {
    const buffer = new LiveBuffer()

    buffer.add([at(1000), at(1250)])
    buffer.add([at(1250), at(1000), at(1500)])

    expect(buffer.series().times).toStrictEqual([1, 1.25, 1.5])
    expect(buffer.newest).toBe(1500)
  })

  it("drops samples that have left the window", () => {
    const buffer = new LiveBuffer()
    const newest = LIVE_WINDOW_MS * 3

    buffer.add([at(1000), at(newest - LIVE_WINDOW_MS), at(newest)])

    // The oldest left the window; the gap after the next one is marked.
    expect(buffer.series().times).toStrictEqual([
      (newest - LIVE_WINDOW_MS) / 1000,
      (newest - LIVE_WINDOW_MS + 1500) / 1000,
      newest / 1000,
    ])
  })

  it("breaks the line across a gap rather than drawing over it", () => {
    const buffer = new LiveBuffer()

    buffer.add([at(1000, 5), at(1250, 6), at(9000, 7)])

    const series = buffer.series()

    expect(series.times).toStrictEqual([1, 1.25, 2.75, 9])
    expect(series.cpu).toStrictEqual([5, 6, null, 7])
  })
})
