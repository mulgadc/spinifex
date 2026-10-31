import { afterEach, describe, expect, it, vi } from "vitest"

import {
  LiveClock,
  type LiveStatus,
  watchInstanceMetrics,
} from "./live-metrics"
import { LIVE_DELAY_MS } from "./live-sample"
import { SignedFetchError, signedStream } from "./signed-fetch"

vi.mock("./signed-fetch", async (importOriginal) => ({
  ...(await importOriginal<typeof import("./signed-fetch")>()),
  signedStream: vi.fn(),
}))

const stream = vi.mocked(signedStream)

function segment(...times: number[]): Response {
  const body = times
    .map(
      (t) =>
        `event: sample\ndata: ${JSON.stringify({ t, cpu: 1, netIn: 0, netOut: 0, pktIn: 0, pktOut: 0 })}\n\n`,
    )
    .join("")

  return new Response(body, {
    headers: { "Content-Type": "text/event-stream" },
  })
}

describe("LiveClock", () => {
  it("takes the fastest arrival as the link's offset", () => {
    const clock = new LiveClock()

    clock.observe(1000, 1300)
    clock.observe(1250, 1350)
    clock.observe(1500, 1900)

    expect(clock.now(2000)).toBe(2000 - 100 - LIVE_DELAY_MS)
  })
})

describe("watchInstanceMetrics", () => {
  afterEach(() => {
    vi.useRealTimers()
    stream.mockReset()
  })

  it("reconnects from the newest sample when a segment ends", async () => {
    const controller = new AbortController()
    const received: number[] = []

    stream
      .mockResolvedValueOnce(segment(1000, 1250))
      .mockImplementationOnce(async () => {
        controller.abort()

        return segment(1500)
      })

    await watchInstanceMetrics(
      "i-0123456789abcdef0",
      0,
      {
        onSamples: (samples) => received.push(...samples.map((s) => s.t)),
        onStatus: () => {},
      },
      controller.signal,
    )

    expect(stream.mock.calls.map(([options]) => options.params)).toStrictEqual([
      { InstanceId: "i-0123456789abcdef0" },
      { InstanceId: "i-0123456789abcdef0", Since: "1250" },
    ])
    expect(received).toStrictEqual([1000, 1250, 1500])
  })

  it("reports an instance with no sampler apart from other failures", async () => {
    vi.useFakeTimers()

    const controller = new AbortController()
    const statuses: LiveStatus[] = []

    stream
      .mockRejectedValueOnce(
        new SignedFetchError("gone", "InvalidInstanceID.NotFound", 400),
      )
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockImplementationOnce(async () => {
        controller.abort()

        return segment()
      })

    const done = watchInstanceMetrics(
      "i-0123456789abcdef0",
      0,
      { onSamples: () => {}, onStatus: (status) => statuses.push(status) },
      controller.signal,
    )

    await vi.runAllTimersAsync()
    await done

    expect(statuses).toStrictEqual(["unavailable", "error", "live"])
  })

  it("backs off after a segment that delivered nothing", async () => {
    vi.useFakeTimers()

    const controller = new AbortController()

    stream.mockResolvedValueOnce(segment()).mockImplementationOnce(async () => {
      controller.abort()

      return segment()
    })

    const done = watchInstanceMetrics(
      "i-0123456789abcdef0",
      0,
      { onSamples: () => {}, onStatus: () => {} },
      controller.signal,
    )

    await vi.advanceTimersByTimeAsync(999)
    expect(stream).toHaveBeenCalledOnce()

    await vi.advanceTimersByTimeAsync(1)
    await done
    expect(stream).toHaveBeenCalledTimes(2)
  })

  it("stops on a gateway that does not know the action", async () => {
    const statuses: LiveStatus[] = []

    stream.mockRejectedValue(
      new SignedFetchError("unknown", "InvalidAction", 400),
    )

    await watchInstanceMetrics(
      "i-0123456789abcdef0",
      0,
      { onSamples: () => {}, onStatus: (status) => statuses.push(status) },
      new AbortController().signal,
    )

    expect(stream).toHaveBeenCalledOnce()
    expect(statuses).toStrictEqual(["unsupported"])
  })
})
