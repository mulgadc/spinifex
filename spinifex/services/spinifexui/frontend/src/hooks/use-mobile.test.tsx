import { act, renderHook } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

let changeListener: (() => void) | null = null

function mockMatchMedia(matches: boolean) {
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: vi.fn().mockReturnValue({
      matches,
      // oxlint-disable-next-line promise/prefer-await-to-callbacks -- mocking matchMedia DOM API
      addEventListener: (_event: string, cb: () => void) => {
        changeListener = cb
      },
      removeEventListener: () => {
        changeListener = null
      },
    }),
  })
}

describe("useIsMobile", () => {
  beforeEach(() => {
    vi.resetModules()
    changeListener = null
  })

  afterEach(() => {
    changeListener = null
  })

  it("returns false on desktop viewport", async () => {
    mockMatchMedia(false)
    const { useIsMobile } = await import("./use-mobile")
    const { result } = renderHook(() => useIsMobile())
    expect(result.current).toBeFalsy()
  })

  it("returns true on mobile viewport", async () => {
    mockMatchMedia(true)
    const { useIsMobile } = await import("./use-mobile")
    const { result } = renderHook(() => useIsMobile())
    expect(result.current).toBeTruthy()
  })

  it("updates when viewport changes", async () => {
    const mqlMock = {
      matches: false,
      // oxlint-disable-next-line promise/prefer-await-to-callbacks -- mocking matchMedia DOM API
      addEventListener: (_event: string, cb: () => void) => {
        changeListener = cb
      },
      removeEventListener: () => {
        changeListener = null
      },
    }

    Object.defineProperty(window, "matchMedia", {
      writable: true,
      value: vi.fn().mockReturnValue(mqlMock),
    })

    const { useIsMobile } = await import("./use-mobile")
    const { result } = renderHook(() => useIsMobile())
    expect(result.current).toBeFalsy()

    // Simulate viewport shrink
    mqlMock.matches = true
    act(() => {
      changeListener?.()
    })
    expect(result.current).toBeTruthy()
  })

  it("cleans up listener on unmount", async () => {
    mockMatchMedia(false)
    const { useIsMobile } = await import("./use-mobile")
    const { unmount } = renderHook(() => useIsMobile())

    expect(changeListener).not.toBeNull()
    unmount()
    expect(changeListener).toBeNull()
  })
})
