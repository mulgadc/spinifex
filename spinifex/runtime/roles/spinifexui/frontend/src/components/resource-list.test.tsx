import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { ResourceList } from "./resource-list"

interface Item {
  id?: string
  name: string
}

function renderList(items: Item[]) {
  return render(
    <ResourceList
      empty="No things found."
      getKey={(item) => item.id}
      items={items}
      renderItem={(item, id) => (
        <span>
          {item.name}:{id}
        </span>
      )}
    />,
  )
}

describe("ResourceList", () => {
  it("renders each item with its key", () => {
    renderList([
      { id: "a-1", name: "alpha" },
      { id: "b-2", name: "beta" },
    ])
    expect(screen.getByText("alpha:a-1")).toBeInTheDocument()
    expect(screen.getByText("beta:b-2")).toBeInTheDocument()
    expect(screen.queryByText("No things found.")).not.toBeInTheDocument()
  })

  it("skips items without a key", () => {
    renderList([{ name: "orphan" }, { id: "b-2", name: "beta" }])
    expect(screen.queryByText(/orphan/)).not.toBeInTheDocument()
    expect(screen.getByText("beta:b-2")).toBeInTheDocument()
  })

  it("renders the empty message when there are no items", () => {
    renderList([])
    expect(screen.getByText("No things found.")).toBeInTheDocument()
  })
})
