import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  LARGE_UPLOAD_BYTES,
  SLOW_UPLOAD_MS,
  type StartTransfer,
  type Transfer,
  type TransferResult,
  UploadStore,
} from "./uploads"

// A transfer the test drives: it reports progress, then finishes or fails
// when told to.
class FakeTransfer implements Transfer {
  readonly key: string
  readonly abort = vi.fn(async () => {
    this.#reject(new Error("aborted"))
  })

  readonly #result: Promise<TransferResult>
  #report: (progress: { loaded?: number }) => void = noProgress
  #resolve: (result: TransferResult) => void = noProgress
  #reject: (error: Error) => void = noProgress

  constructor(key: string) {
    this.key = key
    // oxlint-disable-next-line promise/avoid-new -- the test settles each transfer by hand
    this.#result = new Promise((resolve, reject) => {
      this.#resolve = resolve
      this.#reject = reject
    })
  }

  async done() {
    return await this.#result
  }

  on(
    _event: "httpUploadProgress",
    listener: (progress: { loaded?: number }) => void,
  ) {
    this.#report = listener
  }

  progress(loaded: number) {
    this.#report({ loaded })
  }

  finish() {
    this.#resolve({ Key: this.key })
  }

  fail(message: string) {
    this.#reject(new Error(message))
  }
}

function noProgress() {
  // Replaced once the store listens.
}

function file(name: string, size: number) {
  return { name, size, type: "" } as File
}

function leaving() {
  const event = new Event("beforeunload", { cancelable: true })
  window.dispatchEvent(event)

  return event.defaultPrevented
}

let clock = 0
let store: UploadStore
let started: FakeTransfer[] = []

const start: StartTransfer = ({ key }) => {
  const transfer = new FakeTransfer(key)
  started.push(transfer)

  return transfer
}

async function settle() {
  await vi.advanceTimersByTimeAsync(0)
}

describe("UploadStore", () => {
  beforeEach(() => {
    vi.useFakeTimers()
    clock = 0
    started = []
    store = new UploadStore(start, () => clock)
  })

  afterEach(() => {
    store.cancel()
    vi.useRealTimers()
  })

  it("keeps a small, quick upload on the button", async () => {
    const onUploaded = vi.fn()
    store.add("photos", "2026/", [file("a.jpg", 1000)], onUploaded)

    expect(store.getSnapshot()).toMatchObject({
      active: true,
      open: false,
      bucket: "photos",
      prefix: "2026/",
      total: 1000,
    })
    expect(started[0]?.key).toBe("2026/a.jpg")

    started[0]?.finish()
    await settle()
    await vi.advanceTimersByTimeAsync(SLOW_UPLOAD_MS)

    expect(onUploaded).toHaveBeenCalledOnce()
    expect(store.getSnapshot()).toMatchObject({ active: false, open: false })
    expect(store.getSnapshot().items[0]?.status).toBe("done")
  })

  it("opens the dialog at once for a large batch", () => {
    store.add("b", "", [file("disk.img", LARGE_UPLOAD_BYTES)], vi.fn())

    expect(store.getSnapshot().open).toBeTruthy()
  })

  it("opens the dialog for a small upload that is still going", async () => {
    store.add("b", "", [file("a.txt", 1000)], vi.fn())
    await vi.advanceTimersByTimeAsync(SLOW_UPLOAD_MS - 1)

    expect(store.getSnapshot().open).toBeFalsy()

    await vi.advanceTimersByTimeAsync(1)

    expect(store.getSnapshot().open).toBeTruthy()
  })

  it("stays hidden once the user hides it", async () => {
    store.add("b", "", [file("disk.img", LARGE_UPLOAD_BYTES)], vi.fn())
    store.show(false)
    store.add("b", "", [file("more.img", 10)], vi.fn())
    await vi.advanceTimersByTimeAsync(SLOW_UPLOAD_MS)

    expect(store.getSnapshot()).toMatchObject({ open: false, active: true })

    store.show(true)

    expect(store.getSnapshot().open).toBeTruthy()
  })

  it("runs three files at a time and starts the next as one finishes", async () => {
    store.add(
      "b",
      "",
      ["1", "2", "3", "4", "5"].map((name) => file(name, 10)),
      vi.fn(),
    )

    expect(started.map(({ key }) => key)).toStrictEqual(["1", "2", "3"])

    started[1]?.finish()
    await settle()

    expect(started.map(({ key }) => key)).toStrictEqual(["1", "2", "3", "4"])
  })

  it("adds up progress and measures the rate", async () => {
    store.add("b", "", [file("a", 4000), file("b", 6000)], vi.fn())

    clock = 1000
    started[0]?.progress(1000)
    started[1]?.progress(1000)
    await vi.advanceTimersByTimeAsync(100)

    clock = 2000
    started[0]?.progress(2000)
    started[1]?.progress(3000)
    await vi.advanceTimersByTimeAsync(100)

    expect(store.getSnapshot()).toMatchObject({
      loaded: 5000,
      total: 10_000,
      // 5,000 bytes in the two seconds since the batch began.
      rate: 2500,
    })
    expect(store.getSnapshot().items.map(({ loaded }) => loaded)).toStrictEqual(
      [2000, 3000],
    )
  })

  it("cancels one file, and leaves it out of the totals", async () => {
    store.add("b", "", [file("a", 100), file("b", 300)], vi.fn())
    const [first] = store.getSnapshot().items

    store.cancel(first?.id)
    await settle()

    expect(started[0]?.abort).toHaveBeenCalledOnce()
    expect(store.getSnapshot().items.map(({ status }) => status)).toStrictEqual(
      ["cancelled", "uploading"],
    )
    expect(store.getSnapshot().total).toBe(300)
  })

  it("cancels everything left, including what is queued", async () => {
    store.add(
      "b",
      "",
      ["1", "2", "3", "4"].map((name) => file(name, 10)),
      vi.fn(),
    )
    store.cancel()
    await settle()

    expect(started).toHaveLength(3)
    expect(store.getSnapshot().active).toBeFalsy()
    expect(
      store.getSnapshot().items.every(({ status }) => status === "cancelled"),
    ).toBeTruthy()
  })

  it("reports a failure and retries it", async () => {
    store.add("b", "", [file("a", 10)], vi.fn())
    started[0]?.fail("Access Denied")
    await settle()

    expect(store.getSnapshot().items[0]).toMatchObject({
      status: "failed",
      error: "Access Denied",
    })
    expect(store.getSnapshot().active).toBeFalsy()

    store.retry()

    expect(started).toHaveLength(2)
    expect(store.getSnapshot().items[0]).toMatchObject({
      status: "uploading",
      error: undefined,
    })
  })

  it("asks before the page is left while uploading", async () => {
    store.add("b", "", [file("a", 10)], vi.fn())

    expect(leaving()).toBeTruthy()

    started[0]?.finish()
    await settle()

    expect(leaving()).toBeFalsy()
  })

  it("adds to a running batch and replaces a finished one", async () => {
    store.add("b", "one/", [file("a", 10)], vi.fn())
    store.add("b", "two/", [file("b", 10)], vi.fn())

    expect(store.getSnapshot().items).toHaveLength(2)
    expect(store.getSnapshot().prefix).toBe("one/")

    started[0]?.finish()
    started[1]?.finish()
    await settle()
    store.add("b", "three/", [file("c", 10)], vi.fn())

    expect(store.getSnapshot().items.map(({ key }) => key)).toStrictEqual([
      "three/c",
    ])
  })

  it("clears a finished batch when its dialog closes", async () => {
    store.add("b", "", [file("a", LARGE_UPLOAD_BYTES)], vi.fn())
    started[0]?.finish()
    await settle()
    store.show(false)

    expect(store.getSnapshot()).toMatchObject({ items: [], open: false })
  })
})
