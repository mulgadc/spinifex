import {
  type LiveSample,
  EventStreamParser,
  LIVE_DELAY_MS,
  liveSampleSchema,
} from "./live-sample"
import { SignedFetchError, signedStream } from "./signed-fetch"

// Reconnecting straight away is the normal end of a stream segment; failures,
// and segments that end having delivered nothing, back off.
const BACKOFF_MS = [0, 1000, 2000, 5000]

export type LiveStatus =
  | "connecting"
  | "live"
  | "unavailable"
  | "unsupported"
  | "error"

// What a failed request means for the view. A gateway without the action will
// not grow one by retrying, so that one stops the loop.
function statusOf(code: string | undefined, absent: string): LiveStatus {
  if (code === "InvalidAction") {
    return "unsupported"
  }

  return code === absent ? "unavailable" : "error"
}

// Maps the browser's clock onto the host's. The smallest arrival-minus-sample
// time seen is the link's best case, so the newest sample lands at the edge.
export class LiveClock {
  #offset: number | undefined

  observe(sampleMs: number, arrivalMs: number) {
    const offset = arrivalMs - sampleMs

    if (this.#offset === undefined || offset < this.#offset) {
      this.#offset = offset
    }
  }

  // The host time the chart's right edge should show.
  now(browserMs: number): number {
    return browserMs - (this.#offset ?? 0) - LIVE_DELAY_MS
  }
}

export interface LiveWatchHandlers<T = LiveSample> {
  onSamples: (samples: T[]) => void
  onStatus: (status: LiveStatus) => void
}

async function pause(ms: number, signal: AbortSignal) {
  if (ms === 0 || signal.aborted) {
    return
  }

  // oxlint-disable-next-line promise/avoid-new -- a timer that an abort cuts short
  await new Promise<void>((resolve) => {
    const timer = window.setTimeout(resolve, ms)

    signal.addEventListener(
      "abort",
      () => {
        window.clearTimeout(timer)
        resolve()
      },
      { once: true },
    )
  })
}

// Reads one stream segment to its end, handing on samples as they arrive.
async function readSegment<T>(
  response: Response,
  parser: EventStreamParser<T>,
  onSamples: (samples: T[]) => void,
) {
  if (!response.body) {
    return
  }

  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader()

  for (;;) {
    // oxlint-disable-next-line no-await-in-loop -- each chunk follows the last
    const { done, value } = await reader.read()

    if (done) {
      return
    }

    const samples = parser.push(value)

    if (samples.length > 0) {
      onSamples(samples)
    }
  }
}

// Streams an instance's live statistics until the signal aborts, reconnecting
// from the newest sample whenever a segment ends or fails.
export async function watchInstanceMetrics(
  instanceId: string,
  since: number,
  handlers: LiveWatchHandlers,
  signal: AbortSignal,
) {
  await watchLive(
    {
      action: "GetInstanceMetrics",
      params: { InstanceId: instanceId },
      absent: "InvalidInstanceID.NotFound",
      parser: () => new EventStreamParser(liveSampleSchema),
    },
    since,
    handlers,
    signal,
  )
}

export interface LiveSource<T> {
  action: string
  params: Record<string, string>
  // The error code for a source nobody is sampling, which is worth retrying.
  absent: string
  parser: () => EventStreamParser<T>
}

export async function watchLive<T extends { t: number }>(
  source: LiveSource<T>,
  since: number,
  handlers: LiveWatchHandlers<T>,
  signal: AbortSignal,
) {
  let newest = since
  let failures = 0
  let delivered = false

  const onSamples = (samples: T[]) => {
    delivered = true
    newest = samples.at(-1)?.t ?? newest
    handlers.onSamples(samples)
  }

  while (!signal.aborted) {
    try {
      // oxlint-disable-next-line no-await-in-loop -- one segment at a time
      const response = await signedStream({
        action: source.action,
        params: {
          ...source.params,
          ...(newest > 0 && { Since: String(newest) }),
        },
        signal,
      })

      handlers.onStatus("live")
      delivered = false
      // oxlint-disable-next-line no-await-in-loop
      await readSegment(response, source.parser(), onSamples)
      failures = delivered ? 0 : failures + 1
    } catch (error) {
      if (signal.aborted) {
        return
      }

      const status = statusOf(
        error instanceof SignedFetchError ? error.name : undefined,
        source.absent,
      )

      handlers.onStatus(status)

      if (status === "unsupported") {
        return
      }

      failures += 1
    }

    // oxlint-disable-next-line no-await-in-loop
    await pause(
      BACKOFF_MS[Math.min(failures, BACKOFF_MS.length - 1)] ?? 0,
      signal,
    )
  }
}
