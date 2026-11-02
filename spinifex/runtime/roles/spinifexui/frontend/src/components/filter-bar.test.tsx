import { fireEvent, render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { FilterBar } from "./filter-bar"

const CHOICES = [
  { value: "all", label: "All", count: 3 },
  { value: "manual", label: "Manual", count: 2 },
] as const

function renderBar(query = "", choice: "all" | "manual" = "all") {
  const onQueryChange = vi.fn()
  const onChoiceChange = vi.fn()
  render(
    <FilterBar
      choice={choice}
      choices={CHOICES}
      choicesLabel="Type"
      onChoiceChange={onChoiceChange}
      onQueryChange={onQueryChange}
      placeholder="Filter"
      query={query}
      searchLabel="Filter things"
    />,
  )

  return { onQueryChange, onChoiceChange }
}

describe("FilterBar", () => {
  it("offers Clear filters only while something is filtered", () => {
    renderBar()
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull()
  })

  it("clears both the search and the choice back to the first", () => {
    const { onQueryChange, onChoiceChange } = renderBar("db", "manual")
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }))
    expect(onQueryChange).toHaveBeenCalledWith("")
    expect(onChoiceChange).toHaveBeenCalledWith("all")
  })

  it("keeps the choice when its pressed item is clicked again", () => {
    const { onChoiceChange } = renderBar()
    fireEvent.click(screen.getByRole("button", { name: "All 3" }))
    expect(onChoiceChange).not.toHaveBeenCalled()
  })

  it("passes on the item chosen", () => {
    const { onChoiceChange } = renderBar()
    fireEvent.click(screen.getByRole("button", { name: "Manual 2" }))
    expect(onChoiceChange).toHaveBeenCalledWith("manual")
  })
})
