import { z } from "zod"

// The live charts' window, and how far behind the newest possible sample they
// draw: one sample period plus slack, so a sample landing late over a slow link
// is already on the chart when the edge reaches it.
export const LIVE_WINDOW_MS = 5 * 60 * 1000
export const LIVE_SAMPLE_MS = 500
export const LIVE_DELAY_MS = 1000

const sampleSchema = z.object({
  t: z.number(),
  cpu: z.number(),
  netIn: z.number(),
  netOut: z.number(),
  pktIn: z.number(),
  pktOut: z.number(),
})

export type LiveSample = z.infer<typeof sampleSchema>

export type LiveStatus = "connecting" | "live" | "unavailable" | "error"

// Reads text/event-stream framing as it arrives, keeping a partial event until
// its blank line, and returns the sample events in each chunk.
export class EventStreamParser {
  #buffer = ""

  push(chunk: string): LiveSample[] {
    this.#buffer += chunk.replaceAll("\r\n", "\n")
    const events = this.#buffer.split("\n\n")
    this.#buffer = events.pop() ?? ""

    return events.flatMap((event) => parseEvent(event))
  }
}

function parseEvent(event: string): LiveSample[] {
  let type = "message"
  const data: string[] = []

  for (const line of event.split("\n")) {
    if (line.startsWith("event:")) {
      type = line.slice(6).trim()
    } else if (line.startsWith("data:")) {
      data.push(line.slice(5).trimStart())
    }
  }

  if (type !== "sample" || data.length === 0) {
    return []
  }

  try {
    const parsed = sampleSchema.safeParse(JSON.parse(data.join("\n")))

    return parsed.success ? [parsed.data] : []
  } catch {
    return []
  }
}
