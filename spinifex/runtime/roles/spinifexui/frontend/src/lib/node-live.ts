import { z } from "zod"

import { LiveBuffer, type LiveSeries } from "./live-buffer"
import { type LiveWatchHandlers, watchLive } from "./live-metrics"
import { EventStreamParser, type LiveSample } from "./live-sample"

const planeSchema = z.object({
  netIn: z.number(),
  netOut: z.number(),
  pktIn: z.number(),
  pktOut: z.number(),
})

const nodeSampleSchema = z.object({
  t: z.number(),
  cpu: z.number(),
  mem: z.number(),
  load1: z.number(),
  // Keyed by the planes a set of NICs carries, e.g. "wan" or "lan+vpc".
  planes: z.record(z.string(), planeSchema),
})

export type NodeLiveSample = z.infer<typeof nodeSampleSchema>

// Streams one node's host load, as watchInstanceMetrics does an instance's.
export async function watchNodeMetrics(
  node: string,
  since: number,
  handlers: LiveWatchHandlers<NodeLiveSample>,
  signal: AbortSignal,
) {
  await watchLive(
    {
      action: "GetNodeMetrics",
      params: { NodeId: node },
      absent: "InvalidParameterValue",
      parser: () => new EventStreamParser(nodeSampleSchema),
    },
    since,
    handlers,
    signal,
  )
}

export interface NodeLiveSeries {
  // cpu is the host's CPU; memory reuses the cpu field of its own series.
  cpu: LiveSeries
  memory: LiveSeries
  planes: [string, LiveSeries][]
}

// Splits each host sample into the per-chart buffers the instance view
// already knows how to draw, so gaps and the window behave the same.
export class NodeLiveBuffer {
  readonly #cpu = new LiveBuffer()
  readonly #memory = new LiveBuffer()
  readonly #planes = new Map<string, LiveBuffer>()

  add(samples: readonly NodeLiveSample[]) {
    const blank = { netIn: 0, netOut: 0, pktIn: 0, pktOut: 0 }

    this.#cpu.add(
      samples.map((s): LiveSample => ({ ...blank, t: s.t, cpu: s.cpu })),
    )
    this.#memory.add(
      samples.map((s): LiveSample => ({ ...blank, t: s.t, cpu: s.mem })),
    )

    for (const sample of samples) {
      for (const [plane, rates] of Object.entries(sample.planes)) {
        let buffer = this.#planes.get(plane)

        if (!buffer) {
          buffer = new LiveBuffer()
          this.#planes.set(plane, buffer)
        }

        buffer.add([{ t: sample.t, cpu: 0, ...rates }])
      }
    }
  }

  series(): NodeLiveSeries {
    return {
      cpu: this.#cpu.series(),
      memory: this.#memory.series(),
      planes: [...this.#planes.entries()]
        .map(([plane, buffer]): [string, LiveSeries] => [
          plane,
          buffer.series(),
        ])
        .toSorted(([a], [b]) => a.localeCompare(b)),
    }
  }
}
