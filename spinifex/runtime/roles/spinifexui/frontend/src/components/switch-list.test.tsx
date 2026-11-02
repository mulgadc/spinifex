import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { SecurityGroupPicker } from "./security-group-picker"
import { SwitchList } from "./switch-list"

const ITEMS = [
  { id: "a", label: "a" },
  { id: "b", label: "b" },
]

describe("SwitchList", () => {
  it("grows with its rows unless asked to scroll", () => {
    render(
      <SwitchList
        emptyText="None"
        itemLabel="Item"
        items={ITEMS}
        onChange={vi.fn()}
        selected={[]}
      />,
    )
    expect(screen.getByRole("list")).not.toHaveClass("overflow-y-auto")
  })

  it("caps a scrolling list and scrolls the rest", () => {
    render(
      <SwitchList
        emptyText="None"
        itemLabel="Item"
        items={ITEMS}
        onChange={vi.fn()}
        scroll
        selected={[]}
      />,
    )
    expect(screen.getByRole("list")).toHaveClass("max-h-64", "overflow-y-auto")
  })

  it("scrolls every security group picker", () => {
    render(
      <SecurityGroupPicker
        emptyText="None"
        groups={[{ GroupId: "sg-1", GroupName: "default" }]}
        onChange={vi.fn()}
        selected={[]}
      />,
    )
    expect(screen.getByRole("list")).toHaveClass("overflow-y-auto")
  })
})
