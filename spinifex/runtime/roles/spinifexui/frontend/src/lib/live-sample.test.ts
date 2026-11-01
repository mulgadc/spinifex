import { describe, expect, it } from "vitest"

import { EventStreamParser, liveSampleSchema } from "./live-sample"

const sample = { t: 1000, cpu: 12.5, netIn: 1, netOut: 2, pktIn: 3, pktOut: 4 }
const event = `id: 1000\nevent: sample\ndata: ${JSON.stringify(sample)}\n\n`

describe("EventStreamParser", () => {
  it("holds a split event until its blank line arrives", () => {
    const parser = new EventStreamParser(liveSampleSchema)

    expect(parser.push(event.slice(0, 20))).toStrictEqual([])
    expect(parser.push(event.slice(20, -1))).toStrictEqual([])
    expect(parser.push(event.slice(-1))).toStrictEqual([sample])
  })

  it("reads CRLF framing and several events in one chunk", () => {
    const parser = new EventStreamParser(liveSampleSchema)
    const second = { ...sample, t: 1250 }
    const chunk = `${event}event: sample\r\ndata: ${JSON.stringify(second)}\r\n\r\n`

    expect(parser.push(chunk)).toStrictEqual([sample, second])
  })

  it("skips keep-alives, other events and malformed data", () => {
    const parser = new EventStreamParser(liveSampleSchema)

    const chunk = [
      ": keep-alive\n\n",
      `event: other\ndata: ${JSON.stringify(sample)}\n\n`,
      "event: sample\ndata: {not json\n\n",
      'event: sample\ndata: {"t":1}\n\n',
      event,
    ].join("")

    expect(parser.push(chunk)).toStrictEqual([sample])
  })
})
