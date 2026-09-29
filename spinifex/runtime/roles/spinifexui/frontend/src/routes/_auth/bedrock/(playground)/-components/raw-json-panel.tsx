import { ChevronDown } from "lucide-react"
import { useState } from "react"

import { cn } from "@/lib/utils"

interface RawJsonPanelProps {
  request: unknown
  response: unknown
}

export function RawJsonPanel({ request, response }: RawJsonPanelProps) {
  const [expanded, setExpanded] = useState(false)

  return (
    <div className="rounded-md border border-border">
      <button
        aria-expanded={expanded}
        className="flex w-full items-center gap-2 px-3 py-2 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
        onClick={() => {
          setExpanded((prev) => !prev)
        }}
        type="button"
      >
        <span>Raw request / response</span>
        <ChevronDown
          className={cn(
            "ml-auto size-3.5 transition-transform",
            expanded && "rotate-180",
          )}
        />
      </button>
      {expanded && (
        <div className="space-y-3 border-t border-border p-3">
          <div>
            <h4 className="mb-1 text-[0.625rem] font-medium text-muted-foreground">
              Request
            </h4>
            <pre className="max-h-80 overflow-auto rounded bg-muted p-2 font-mono text-xs break-words whitespace-pre-wrap">
              {request ? JSON.stringify(request, null, 2) : "(no request yet)"}
            </pre>
          </div>
          <div>
            <h4 className="mb-1 text-[0.625rem] font-medium text-muted-foreground">
              Response
            </h4>
            <pre className="max-h-80 overflow-auto rounded bg-muted p-2 font-mono text-xs break-words whitespace-pre-wrap">
              {response
                ? JSON.stringify(response, null, 2)
                : "(no response yet)"}
            </pre>
          </div>
        </div>
      )}
    </div>
  )
}
