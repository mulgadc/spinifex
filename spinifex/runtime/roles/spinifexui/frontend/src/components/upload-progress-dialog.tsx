import {
  CircleCheck,
  CircleX,
  Clock,
  LoaderCircle,
  type LucideIcon,
  RotateCcw,
  X,
} from "lucide-react"
import { useRef } from "react"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Progress } from "@/components/ui/progress"
import {
  formatBitRate,
  formatElapsed,
  formatRemaining,
  percentOf,
} from "@/lib/transfer"
import { type UploadItem, uploads, useUploads } from "@/lib/uploads"
import { formatSize } from "@/lib/utils"

function plural(count: number, one: string, many: string) {
  return `${count} ${count === 1 ? one : many}`
}

const STATUS_ICONS: Record<
  UploadItem["status"],
  { icon: LucideIcon; label: string; className: string }
> = {
  queued: { icon: Clock, label: "Queued", className: "text-muted-foreground" },
  uploading: {
    icon: LoaderCircle,
    label: "Uploading",
    className: "animate-spin text-muted-foreground motion-reduce:animate-none",
  },
  done: { icon: CircleCheck, label: "Uploaded", className: "text-success" },
  failed: { icon: CircleX, label: "Failed", className: "text-destructive" },
  cancelled: {
    icon: X,
    label: "Cancelled",
    className: "text-muted-foreground",
  },
}

function StatusIcon({ status }: { status: UploadItem["status"] }) {
  const { icon: Icon, label, className } = STATUS_ICONS[status]

  return (
    <Icon aria-label={label} className={`size-3.5 shrink-0 ${className}`} />
  )
}

function ItemRow({ item, prefix }: { item: UploadItem; prefix: string }) {
  const name = item.key.startsWith(prefix)
    ? item.key.slice(prefix.length)
    : item.key

  const percent = percentOf(item.loaded, item.size)
  let detail = formatSize(item.size)

  if (item.status === "uploading") {
    detail = `${percent}%`
  } else if (item.status === "cancelled") {
    detail = "Cancelled"
  }

  return (
    <li className="flex flex-col gap-1 py-2">
      <div className="flex items-center gap-2">
        <StatusIcon status={item.status} />
        <span className="min-w-0 flex-1 truncate" title={item.key}>
          {name}
        </span>
        <span className="text-muted-foreground tabular-nums">{detail}</span>
        {(item.status === "queued" || item.status === "uploading") && (
          <Button
            aria-label={`Cancel ${name}`}
            onClick={() => {
              uploads.cancel(item.id)
            }}
            size="icon-xs"
            variant="ghost"
          >
            <X />
          </Button>
        )}
      </div>
      {item.status === "uploading" && (
        <Progress aria-label={`${name} progress`} value={percent} />
      )}
      {item.error && <p className="text-destructive">{item.error}</p>}
    </li>
  )
}

// What a running or finished batch of uploads is doing. It follows the user
// around the console, and hiding it leaves the progress on the Upload button.
export function UploadProgressDialog() {
  const state = useUploads()
  // Focus starts on Hide or Close, not on the first file's cancel button.
  const primaryRef = useRef<HTMLButtonElement>(null)

  const count = (status: UploadItem["status"]) =>
    state.items.filter((item) => item.status === status).length

  const done = count("done")
  const failed = count("failed")
  const files = state.items.length - count("cancelled")
  const percent = percentOf(state.loaded, state.total)

  const remaining =
    state.rate > 0 ? (state.total - state.loaded) / state.rate : Number.NaN

  let title = `Uploading ${plural(files, "file", "files")}`

  if (!state.active && files === 0) {
    title = state.items.length === 1 ? "Upload cancelled" : "Uploads cancelled"
  } else if (!state.active) {
    title =
      failed > 0
        ? `${failed} of ${plural(files, "upload", "uploads")} failed`
        : `Uploaded ${plural(done, "file", "files")}`
  }

  // Nothing sent for a few seconds reads as stalled once bytes have moved.
  let rate = state.loaded > 0 ? "Stalled" : "Starting…"

  if (state.rate > 0) {
    rate = formatBitRate(state.rate)
  }

  let stats = [
    `${formatSize(state.loaded)} of ${formatSize(state.total)}`,
    rate,
    state.rate > 0 ? formatRemaining(remaining) : "",
  ]

  if (!state.active) {
    const seconds = state.elapsedMs / 1000

    stats = [
      `${formatSize(state.loaded)} in ${formatElapsed(state.elapsedMs)}`,
      seconds > 0 ? `average ${formatBitRate(state.loaded / seconds)}` : "",
    ]
  }

  return (
    <Dialog
      onOpenChange={(open) => {
        uploads.show(open)
      }}
      open={state.open}
    >
      <DialogContent className="sm:max-w-lg" initialFocus={primaryRef}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>
            <span className="block truncate font-mono">
              s3://{state.bucket}/{state.prefix}
            </span>
          </DialogDescription>
        </DialogHeader>

        {files > 0 && (
          <div className="flex flex-col gap-2">
            <div className="flex items-end justify-between gap-4">
              <output
                aria-live="polite"
                className="text-muted-foreground tabular-nums"
              >
                {stats.filter(Boolean).join(" · ")}
              </output>
              <span className="text-2xl font-semibold tabular-nums">
                {percent}%
              </span>
            </div>
            <Progress aria-label="Upload progress" value={percent} />
          </div>
        )}

        <ul className="max-h-64 divide-y overflow-y-auto">
          {state.items.map((item) => (
            <ItemRow item={item} key={item.id} prefix={state.prefix} />
          ))}
        </ul>

        <DialogFooter>
          {state.active ? (
            <>
              <Button
                onClick={() => {
                  uploads.cancel()
                }}
                variant="outline"
              >
                Cancel all
              </Button>
              <Button
                onClick={() => {
                  uploads.show(false)
                }}
                ref={primaryRef}
              >
                Hide
              </Button>
            </>
          ) : (
            <>
              {failed > 0 && (
                <Button
                  onClick={() => {
                    uploads.retry()
                  }}
                  variant="outline"
                >
                  <RotateCcw data-icon="inline-start" />
                  Retry {failed}
                </Button>
              )}
              <Button
                onClick={() => {
                  uploads.show(false)
                }}
                ref={primaryRef}
              >
                Close
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
