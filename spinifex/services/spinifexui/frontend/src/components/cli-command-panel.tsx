import { ChevronDown, Copy, Check, SquareTerminal } from "lucide-react"
import { useState } from "react"

import { useCopyToClipboard } from "@/hooks/use-copy-to-clipboard"
import { cn } from "@/lib/utils"

import { Button } from "./ui/button"

export type CommandPartType = "bin" | "flag" | "value" | "comment" | "variable"

export interface CommandPart {
  type: CommandPartType
  value: string
}

export interface CliCommand {
  label: string
  parts: CommandPart[]
}

interface CliCommandPanelProps {
  commands: CliCommand[]
}

const partStyles: Record<CommandPartType, string> = {
  bin: "text-tactical-cyan font-semibold",
  flag: "text-tactical-amber",
  value: "text-foreground",
  comment: "text-muted-foreground italic",
  variable: "text-tactical-green font-semibold",
}

export function partsToText(commands: CliCommand[]): string {
  return commands
    .map((cmd) => cmd.parts.map((p) => p.value).join(""))
    .join("\n\n")
}

export function CliCommandPanel({ commands }: CliCommandPanelProps) {
  const [expanded, setExpanded] = useState(false)
  const { copied, copy } = useCopyToClipboard()

  if (commands.length === 0) {
    return null
  }

  return (
    <div className="rounded-md border border-border">
      <button
        type="button"
        onClick={() => setExpanded((prev) => !prev)}
        className="flex w-full items-center gap-2 px-3 py-2 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
        aria-expanded={expanded}
      >
        <SquareTerminal className="size-3.5" />
        <span>AWS CLI</span>
        <ChevronDown
          className={cn(
            "ml-auto size-3.5 transition-transform",
            expanded && "rotate-180",
          )}
        />
      </button>
      {expanded && (
        <div className="border-t border-border">
          <div className="flex items-center justify-between border-b border-border px-3 py-1.5">
            <span className="text-[0.625rem] text-muted-foreground">
              {commands.length > 1 ? `${commands.length} commands` : "command"}
            </span>
            <Button
              type="button"
              variant="ghost"
              size="xs"
              onClick={async () => await copy(partsToText(commands))}
            >
              {copied ? (
                <Check className="size-2.5" />
              ) : (
                <Copy className="size-2.5" />
              )}
              {copied ? "Copied" : "Copy"}
            </Button>
          </div>
          <div className="overflow-x-auto p-3">
            {commands.map((cmd, i) => (
              <div key={cmd.label}>
                {commands.length > 1 && (
                  <div className="mb-1 text-[0.625rem] font-medium text-muted-foreground">
                    {cmd.label}
                  </div>
                )}
                <pre className="font-mono text-xs/relaxed">
                  <code>
                    {cmd.parts.map((part, j) => (
                      // oxlint-disable-next-line react/no-array-index-key -- positional inline spans with no stable id
                      <span key={j} className={partStyles[part.type]}>
                        {part.value}
                      </span>
                    ))}
                  </code>
                </pre>
                {i < commands.length - 1 && <div className="my-2" />}
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
