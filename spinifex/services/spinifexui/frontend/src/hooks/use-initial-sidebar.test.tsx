import { renderHook } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

const mockSetOpen = vi.fn()
vi.mock("@/components/ui/sidebar", () => ({
  useSidebar: () => ({ setOpen: mockSetOpen }),
}))

import { useInitialSidebar } from "./use-initial-sidebar"

describe("useInitialSidebar", () => {
  it("calls setOpen with the provided value on first render", () => {
    renderHook(() => useInitialSidebar(true))
    expect(mockSetOpen).toHaveBeenCalledWith(true)
  })

  it("calls setOpen(false) when isOpen is false", () => {
    mockSetOpen.mockClear()
    renderHook(() => useInitialSidebar(false))
    expect(mockSetOpen).toHaveBeenCalledWith(false)
  })

  it("does not call setOpen again on re-render", () => {
    mockSetOpen.mockClear()
    const { rerender } = renderHook(
      ({ isOpen }: { isOpen: boolean }) => useInitialSidebar(isOpen),
      { initialProps: { isOpen: true } },
    )
    expect(mockSetOpen).toHaveBeenCalledOnce()

    rerender({ isOpen: false })
    // Should still only have been called once (the initial mount)
    expect(mockSetOpen).toHaveBeenCalledOnce()
  })
})
