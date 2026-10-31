// A serial console log, coloured the way a terminal reader scans it: the
// kernel timestamp in brand yellow, the subsystem or device that logged the
// line muted, systemd's status markers in their own colours.

// "[    1.310146] pci 0000:00:08.0: BAR 0 ..." and "[  OK  ] Started ...".
const KERNEL_LINE =
  /^(?<stamp>\[\s*\d+\.\d+\])(?<gap>\s+)(?<source>[\w.-]+(?: [\w:.-]+)?:(?= ))?(?<rest>.*)$/u

const SYSTEMD_LINE =
  /^(?<mark>\[\s*(?:OK|FAILED|DEPEND|TIME|WARN)\s*\])(?<rest>.*)$/u

export type ConsoleTone = "stamp" | "source" | "ok" | "fail" | "plain"

export interface ConsoleSegment {
  tone: ConsoleTone
  text: string
}

export function parseConsoleLine(line: string): ConsoleSegment[] {
  const kernel = KERNEL_LINE.exec(line)?.groups

  if (kernel) {
    return [
      { tone: "stamp", text: kernel.stamp ?? "" },
      { tone: "plain", text: kernel.gap ?? "" },
      ...(kernel.source
        ? [{ tone: "source" as const, text: kernel.source }]
        : []),
      { tone: "plain", text: kernel.rest ?? "" },
    ]
  }

  const systemd = SYSTEMD_LINE.exec(line)?.groups

  if (systemd) {
    const mark = systemd.mark ?? ""

    return [
      { tone: mark.includes("OK") ? "ok" : "fail", text: mark },
      { tone: "plain", text: systemd.rest ?? "" },
    ]
  }

  return [{ tone: "plain", text: line }]
}

interface ConsoleOutputProps {
  output: string
}

// Follows the theme; the console tokens pick a shade of each tone that reads
// on that theme's background.
export function ConsoleOutput({ output }: ConsoleOutputProps) {
  const lines = output.split(/\r?\n/u)

  return (
    <pre
      aria-label="Console output"
      className="max-h-96 overflow-auto rounded-md bg-console p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap text-console-foreground"
    >
      {lines.map((line, index) => (
        // Lines have no identity beyond their position in the log.
        // oxlint-disable-next-line react/no-array-index-key
        <span className="block" key={index}>
          {parseConsoleLine(line).map((segment, part) => (
            <span
              className="data-[tone=fail]:text-console-fail data-[tone=ok]:text-console-ok data-[tone=source]:text-console-muted data-[tone=stamp]:text-console-stamp"
              data-tone={segment.tone}
              // oxlint-disable-next-line react/no-array-index-key
              key={part}
            >
              {segment.text}
            </span>
          ))}
          {line === "" && " "}
        </span>
      ))}
    </pre>
  )
}
