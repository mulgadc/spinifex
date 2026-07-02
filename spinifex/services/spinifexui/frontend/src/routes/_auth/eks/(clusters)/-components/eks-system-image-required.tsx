import { Check, Copy, RotateCw, TriangleAlert } from "lucide-react"

import { Button } from "@/components/ui/button"
import { useCopyToClipboard } from "@/hooks/use-copy-to-clipboard"

const IMPORT_COMMAND = "spx admin images import --name spinifex-eks-node"

export function EksSystemImageRequired({
  isRechecking,
  onRecheck,
}: {
  isRechecking: boolean
  onRecheck: () => void
}) {
  const { copied, copy } = useCopyToClipboard()

  return (
    <div className="max-w-2xl rounded-md border border-tactical-amber/40 bg-tactical-amber/5 p-6">
      <div className="flex items-start gap-3">
        <TriangleAlert className="mt-0.5 size-5 shrink-0 text-tactical-amber" />
        <div className="space-y-4">
          <div className="space-y-1">
            <h2 className="text-sm font-semibold text-foreground">
              EKS system image not found
            </h2>
            <p className="text-xs text-muted-foreground">
              EKS clusters boot a Spinifex-managed node image (K3s) that is not
              shipped with the platform. Import it before creating a cluster.
            </p>
          </div>

          <div className="flex items-center gap-2 rounded border border-border bg-background px-3 py-2 font-mono text-xs">
            <span className="text-muted-foreground select-none">$</span>
            <span className="flex-1 text-foreground">{IMPORT_COMMAND}</span>
            <button
              aria-label="Copy command"
              className="text-muted-foreground transition-colors hover:text-foreground"
              onClick={() => void copy(IMPORT_COMMAND)}
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
