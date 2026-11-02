import { describe, expect, it } from "vitest"

import { parseConsoleLine } from "./console-output"

describe("parseConsoleLine", () => {
  it("splits a kernel line into timestamp, device and message", () => {
    expect(
      parseConsoleLine("[    1.310146] pci 0000:00:08.0: BAR 0 assigned"),
    ).toStrictEqual([
      { tone: "stamp", text: "[    1.310146]" },
      { tone: "plain", text: " " },
      { tone: "source", text: "pci 0000:00:08.0:" },
      { tone: "plain", text: " BAR 0 assigned" },
    ])
  })

  it("leaves a kernel message with no source prefix as plain text", () => {
    expect(parseConsoleLine("[    0.000000] Linux version 6.12")).toStrictEqual(
      [
        { tone: "stamp", text: "[    0.000000]" },
        { tone: "plain", text: " " },
        { tone: "plain", text: "Linux version 6.12" },
      ],
    )
  })

  it("does not read a colon later in the message as a source", () => {
    const parts = parseConsoleLine(
      "[    2.5] Run /init as init process with args: ok",
    )

    expect(parts.some((part) => part.tone === "source")).toBeFalsy()
  })

  it("marks systemd's OK and FAILED", () => {
    expect(parseConsoleLine("[  OK  ] Started ssh.service.")[0]).toStrictEqual({
      tone: "ok",
      text: "[  OK  ]",
    })
    expect(parseConsoleLine("[FAILED] Failed to start x.")[0]?.tone).toBe(
      "fail",
    )
  })

  it("passes anything else through untouched", () => {
    expect(parseConsoleLine("Ubuntu 26.04 LTS host ttyS0")).toStrictEqual([
      { tone: "plain", text: "Ubuntu 26.04 LTS host ttyS0" },
    ])
  })
})
