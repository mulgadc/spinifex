import { Check, Copy, RotateCw, TriangleAlert } from "lucide-react"

import { Button } from "@/components/ui/button"
import { useCopyToClipboard } from "@/hooks/use-copy-to-clipboard"
import { cn } from "@/lib/utils"

interface SystemImageRequiredProps {
  title: string
  description: string
  importCommand: string
  isRechecking: boolean
  onRecheck: () => void
  // Spacing is left to the call site, whose surrounding layout decides it.
  className?: string
}

// Callout shown when a Spinifex-managed node image is missing, offering the spx
// import command to copy and a recheck once the image has been imported.
export function SystemImageRequired({
  title,
  description,
  importCommand,
  isRechecking,
  onRecheck,
  className,
}: SystemImageRequiredProps) {
  const { copied, copy } = useCopyToClipboard()

  return (
    <div
      className={cn(
        "max-w-2xl rounded-md border border-tactical-amber/40 bg-tactical-amber/5 p-6",
        className,
      )}
    >
      <div className="flex items-start gap-3">
        <TriangleAlert className="mt-0.5 size-5 shrink-0 text-tactical-amber" />
        <div className="space-y-4">
          <div className="space-y-1">
            <h2 className="text-sm font-semibold text-foreground">{title}</h2>
            <p className="text-xs text-muted-foreground">{description}</p>
          </div>

          <div className="flex items-center gap-2 rounded border border-border bg-background px-3 py-2 font-mono text-xs">
            <span className="text-muted-foreground select-none">$</span>
            <span className="flex-1 text-foreground">{importCommand}</span>
            <button
              aria-label="Copy command"
              className="text-muted-foreground transition-colors hover:text-foreground"
              onClick={() => void copy(importCommand)}
              type="button"
            >
              {copied ? (
                <Check className="size-3.5 text-tactical-green" />
              ) : (
                <Copy className="size-3.5" />
              )}
            </button>
          </div>

          <p className="text-xs text-muted-foreground">
            Run from any authenticated spx CLI host. The command downloads and
            registers the published node image, then click Recheck.
          </p>

          <Button
            disabled={isRechecking}
            onClick={onRecheck}
            size="sm"
            variant="outline"
          >
            <RotateCw
              className={isRechecking ? "size-4 animate-spin" : "size-4"}
            />
            Recheck
          </Button>
        </div>
      </div>
    </div>
  )
}
