import { act, renderHook } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { useCopyToClipboard } from "./use-copy-to-clipboard"

describe("useCopyToClipboard", () => {
  beforeEach(() => {
    vi.useFakeTimers()
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: vi.fn().mockResolvedValue(undefined) },
      writable: true,
      configurable: true,
    })
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it("starts with copied as false", () => {
    const { result } = renderHook(() => useCopyToClipboard())
    expect(result.current.copied).toBeFalsy()
  })

  it("sets copied to true after calling copy", async () => {
    const { result } = renderHook(() => useCopyToClipboard())

    await act(async () => {
      await result.current.copy("hello")
    })

    // oxlint-disable-next-line typescript/unbound-method -- vitest mock
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith("hello")
    expect(result.current.copied).toBeTruthy()
  })

  it("resets copied to false after 2 seconds", async () => {
    const { result } = renderHook(() => useCopyToClipboard())

    await act(async () => {
      await result.current.copy("hello")
    })
    expect(result.current.copied).toBeTruthy()

    act(() => {
      vi.advanceTimersByTime(2000)
    })
    expect(result.current.copied).toBeFalsy()
  })

  it("resets the timer on rapid successive copies", async () => {
    const { result } = renderHook(() => useCopyToClipboard())

    await act(async () => {
      await result.current.copy("first")
    })

    // Advance 1.5s, then copy again — should restart the 2s timer
    act(() => {
      vi.advanceTimersByTime(1500)
    })
    expect(result.current.copied).toBeTruthy()

    await act(async () => {
      await result.current.copy("second")
    })

    // 1.5s after second copy — still within the new 2s window
    act(() => {
      vi.advanceTimersByTime(1500)
    })
    expect(result.current.copied).toBeTruthy()

    // 2s after second copy — should reset
    act(() => {
      vi.advanceTimersByTime(500)
    })
    expect(result.current.copied).toBeFalsy()
  })

  it("clears the timer on unmount", async () => {
    const { result, unmount } = renderHook(() => useCopyToClipboard())

    await act(async () => {
      await result.current.copy("hello")
    })

    unmount()

    expect(() => {
      act(() => {
        vi.advanceTimersByTime(2000)
      })
    }).not.toThrow()
  })
})
