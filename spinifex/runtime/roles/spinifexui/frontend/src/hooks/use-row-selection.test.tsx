import { act, renderHook } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { useRowSelection } from "./use-row-selection"

function render(initial: string[]) {
  const closePanel = vi.fn()

  const hook = renderHook(
    ({ rows }: { rows: string[] }) => useRowSelection(rows, false, closePanel),
    { initialProps: { rows: initial } },
  )

  return hook
}

describe("useRowSelection", () => {
  it("forgets a row the filter hides, so clearing the filter does not reselect it", () => {
    const { result, rerender } = render(["i-a", "i-b", "i-c"])

    act(() => {
      result.current.onSelectedChange(new Set(["i-a", "i-b"]))
    })
    rerender({ rows: ["i-a"] })

    expect([...result.current.selected]).toStrictEqual(["i-a"])
    expect(result.current.ids).toStrictEqual(["i-a"])

    rerender({ rows: ["i-a", "i-b", "i-c"] })

    expect(result.current.ids).toStrictEqual(["i-a"])
  })

  it("keeps the selection when the list is unchanged", () => {
    const { result, rerender } = render(["i-a", "i-b"])

    act(() => {
      result.current.onSelectedChange(new Set(["i-b"]))
    })
    const before = result.current.selected
    rerender({ rows: ["i-a", "i-b"] })

    expect(result.current.selected).toBe(before)
  })
})
