import { Upload } from "@aws-sdk/lib-storage"
import { useSyncExternalStore } from "react"

import { createS3UploadClient } from "@/lib/awsClient"
import { currentRate, RATE_WINDOW_MS, type RateSample } from "@/lib/transfer"

export type UploadStatus =
  | "queued"
  | "uploading"
  | "done"
  | "failed"
  | "cancelled"

export interface UploadItem {
  id: string
  bucket: string
  key: string
  size: number
  loaded: number
  status: UploadStatus
  error?: string
}

export interface UploadsState {
  items: readonly UploadItem[]
  // Where the batch began, for the dialog's title.
  bucket: string
  prefix: string
  open: boolean
  active: boolean
  // Bytes across the batch, leaving out what was cancelled.
  loaded: number
  total: number
  // Bytes a second, over the last few seconds.
  rate: number
  elapsedMs: number
}

// A batch this large opens its dialog at once. A smaller one shows its
// progress on the Upload button, and opens the dialog only if still going
// after SLOW_UPLOAD_MS, so a quick upload never flashes a dialog.
export const LARGE_UPLOAD_BYTES = 16 * 1024 * 1024
export const SLOW_UPLOAD_MS = 2000

// Each file is also split into parts sent four at a time.
const CONCURRENT_FILES = 3
const PART_BYTES = 8 * 1024 * 1024
const PART_QUEUE = 4
const PUBLISH_MS = 100
const TICK_MS = 500

// What a finished upload reports; the store needs only that it finished.
export interface TransferResult {
  Key?: string
}

export interface Transfer {
  done: () => Promise<TransferResult>
  abort: () => Promise<void>
  on: (
    event: "httpUploadProgress",
    listener: (progress: { loaded?: number }) => void,
  ) => void
}

export type StartTransfer = (request: {
  bucket: string
  key: string
  file: File
}) => Transfer

function s3Transfer({
  bucket,
  key,
  file,
}: {
  bucket: string
  key: string
  file: File
}): Transfer {
  return new Upload({
    client: createS3UploadClient(),
    params: {
      Bucket: bucket,
      Key: key,
      Body: file,
      ContentType: file.type || undefined,
    },
    partSize: PART_BYTES,
    queueSize: PART_QUEUE,
  })
}

interface Entry extends UploadItem {
  file: File
  transfer?: Transfer
  onUploaded: () => void
}

const IDLE: UploadsState = {
  items: [],
  bucket: "",
  prefix: "",
  open: false,
  active: false,
  loaded: 0,
  total: 0,
  rate: 0,
  elapsedMs: 0,
}

function isActive(entry: Entry) {
  return entry.status === "queued" || entry.status === "uploading"
}

// A multipart upload aborts its parts and the upload itself. One that has
// already finished or failed has nothing left to abort.
async function abortQuietly(transfer: Transfer) {
  try {
    await transfer.abort()
  } catch {
    // Settled already; its own run reports how.
  }
}

// Leaving the page would drop every upload in flight, so the browser asks.
function holdPage(event: BeforeUnloadEvent) {
  event.preventDefault()
}

// The console's uploads, kept outside React so they carry on, and keep their
// progress, while the user browses elsewhere.
export class UploadStore {
  readonly #listeners = new Set<() => void>()
  #state = IDLE
  #entries: Entry[] = []
  #samples: RateSample[] = []
  #bucket = ""
  #prefix = ""
  #open = false
  #hidden = false
  #startedAt = 0
  #finishedAt: number | undefined
  #nextId = 0
  #publishTimer: ReturnType<typeof setTimeout> | undefined
  #slowTimer: ReturnType<typeof setTimeout> | undefined
  // Republishes while running, so a stalled link shows its rate falling.
  #ticker: ReturnType<typeof setInterval> | undefined
  readonly #start: StartTransfer
  readonly #now: () => number

  constructor(
    start: StartTransfer = s3Transfer,
    now = () => performance.now(),
  ) {
    this.#start = start
    this.#now = now
  }

  subscribe = (listener: () => void) => {
    this.#listeners.add(listener)

    return () => {
      this.#listeners.delete(listener)
    }
  }

  getSnapshot = () => this.#state

  add(
    bucket: string,
    prefix: string,
    files: readonly File[],
    onUploaded: () => void,
  ) {
    if (files.length === 0) {
      return
    }

    // A finished batch is replaced; one still running takes the new files.
    if (!this.#entries.some(isActive)) {
      this.#entries = []
      this.#samples = []
      this.#bucket = bucket
      this.#prefix = prefix
      this.#open = false
      this.#hidden = false
      this.#startedAt = this.#now()
      this.#finishedAt = undefined
    }

    for (const file of files) {
      this.#nextId += 1
      this.#entries.push({
        id: String(this.#nextId),
        bucket,
        key: `${prefix}${file.name}`,
        size: file.size,
        loaded: 0,
        status: "queued",
        file,
        onUploaded,
      })
    }

    const total = this.#entries.reduce((sum, entry) => sum + entry.size, 0)

    if (!this.#hidden && total >= LARGE_UPLOAD_BYTES) {
      this.#open = true
    }

    // Counted from the latest files, so a quick follow-up does not open it.
    clearTimeout(this.#slowTimer)
    this.#slowTimer = setTimeout(() => {
      if (!this.#hidden && this.#entries.some(isActive)) {
        this.#open = true
        this.#publish()
      }
    }, SLOW_UPLOAD_MS)

    this.#pump()
    this.#publish()
  }

  // Shows or hides the dialog. Hiding a running batch keeps it going on the
  // button; closing a finished one clears it.
  show(open: boolean) {
    this.#open = open

    if (!open && !this.#entries.some(isActive)) {
      this.#entries = []
      this.#samples = []
    } else if (!open) {
      this.#hidden = true
    }

    this.#publish()
  }

  // Cancels one upload, or with no ID every one still queued or running.
  cancel(id?: string) {
    for (const entry of this.#entries) {
      if ((id === undefined || entry.id === id) && isActive(entry)) {
        entry.status = "cancelled"

        if (entry.transfer) {
          void abortQuietly(entry.transfer)
        }
      }
    }

    this.#publish()
  }

  retry() {
    for (const entry of this.#entries) {
      if (entry.status === "failed") {
        entry.status = "queued"
        entry.loaded = 0
        entry.error = undefined
      }
    }

    this.#finishedAt = undefined
    this.#pump()
    this.#publish()
  }

  #pump() {
    let running = this.#entries.filter(
      (entry) => entry.status === "uploading",
    ).length

    for (const entry of this.#entries) {
      if (running >= CONCURRENT_FILES) {
        break
      }

      if (entry.status === "queued") {
        running += 1
        void this.#run(entry)
      }
    }
  }

  async #run(entry: Entry) {
    entry.status = "uploading"

    try {
      const transfer = this.#start(entry)
      entry.transfer = transfer
      transfer.on("httpUploadProgress", ({ loaded }) => {
        if (loaded !== undefined && entry.status === "uploading") {
          entry.loaded = Math.min(loaded, entry.size)
          this.#schedulePublish()
        }
      })
      await transfer.done()

      if (entry.status === "uploading") {
        entry.status = "done"
        entry.loaded = entry.size
        entry.onUploaded()
      }
    } catch (error) {
      if (entry.status === "uploading") {
        entry.status = "failed"
        entry.error = error instanceof Error ? error.message : String(error)
      }
    } finally {
      entry.transfer = undefined
      this.#pump()
      this.#publish()
    }
  }

  #schedulePublish() {
    if (this.#publishTimer !== undefined) {
      return
    }

    this.#publishTimer = setTimeout(() => {
      this.#publishTimer = undefined
      this.#publish()
    }, PUBLISH_MS)
  }

  #publish() {
    const now = this.#now()

    const counted = this.#entries.filter(
      (entry) => entry.status !== "cancelled",
    )

    const loaded = counted.reduce((sum, entry) => sum + entry.loaded, 0)
    const total = counted.reduce((sum, entry) => sum + entry.size, 0)

    const active = this.#entries.some(isActive)

    this.#samples.push({ at: now, loaded })
    this.#samples = this.#samples.filter(
      (sample) => now - sample.at <= RATE_WINDOW_MS + 1000,
    )

    if (active) {
      this.#finishedAt = undefined
    } else {
      this.#finishedAt ??= now
    }

    if (active !== this.#state.active) {
      if (active) {
        window.addEventListener("beforeunload", holdPage)
        this.#ticker = setInterval(() => {
          this.#publish()
        }, TICK_MS)
      } else {
        window.removeEventListener("beforeunload", holdPage)
        clearInterval(this.#ticker)
        this.#ticker = undefined
      }
    }

    this.#state = {
      items: this.#entries.map(
        ({ id, bucket, key, size, loaded: sent, status, error }) => ({
          id,
          bucket,
          key,
          size,
          loaded: sent,
          status,
          error,
        }),
      ),
      bucket: this.#bucket,
      prefix: this.#prefix,
      open: this.#open && this.#entries.length > 0,
      active,
      loaded,
      total,
      rate: active ? currentRate(this.#samples) : 0,
      elapsedMs: (this.#finishedAt ?? now) - this.#startedAt,
    }

    for (const listener of this.#listeners) {
      listener()
    }
  }
}

export const uploads = new UploadStore()

export function useUploads(): UploadsState {
  return useSyncExternalStore(uploads.subscribe, uploads.getSnapshot)
}
