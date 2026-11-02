import {
  LIVE_DELAY_MS,
  LIVE_SAMPLE_MS,
  LIVE_WINDOW_MS,
  type LiveSample,
} from "./live-sample"

// A step longer than this is a gap: the stream was down, not quiet. Three
// periods, so one late or dropped sample does not break the line.
const GAP_MS = 3 * LIVE_SAMPLE_MS

export interface LiveSeries {
  // Unix seconds, fractional, oldest first.
  times: number[]
  cpu: (number | null)[]
  networkIn: (number | null)[]
  networkOut: (number | null)[]
  packetsIn: (number | null)[]
  packetsOut: (number | null)[]
}

// The samples inside the window, oldest first, with a null after any gap so
// the chart breaks the line rather than drawing across missing time.
export class LiveBuffer {
  #samples: LiveSample[] = []

  add(samples: readonly LiveSample[]) {
    for (const sample of samples) {
      if (sample.t > (this.#samples.at(-1)?.t ?? -1)) {
        this.#samples.push(sample)
      }
    }

    const newest = this.#samples.at(-1)?.t ?? 0
    const oldest = newest - LIVE_WINDOW_MS - 2 * LIVE_DELAY_MS
    const keep = this.#samples.findIndex((sample) => sample.t >= oldest)
    this.#samples = keep === -1 ? [] : this.#samples.slice(keep)
  }

  get newest(): number {
    return this.#samples.at(-1)?.t ?? 0
  }

  series(): LiveSeries {
    const series: LiveSeries = {
      times: [],
      cpu: [],
      networkIn: [],
      networkOut: [],
      packetsIn: [],
      packetsOut: [],
    }

    let previous: number | undefined

    for (const sample of this.#samples) {
      if (previous !== undefined && sample.t - previous > GAP_MS) {
        pushPoint(series, (previous + GAP_MS) / 1000, null)
      }

      pushPoint(series, sample.t / 1000, sample)
      previous = sample.t
    }

    return series
  }
}

function pushPoint(
  series: LiveSeries,
  time: number,
  sample: LiveSample | null,
) {
  series.times.push(time)
  series.cpu.push(sample?.cpu ?? null)
  series.networkIn.push(sample?.netIn ?? null)
  series.networkOut.push(sample?.netOut ?? null)
  series.packetsIn.push(sample?.pktIn ?? null)
  series.packetsOut.push(sample?.pktOut ?? null)
}
