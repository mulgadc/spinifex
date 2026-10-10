import type { Tag } from "@aws-sdk/client-elastic-load-balancing-v2"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { TagsEditor } from "./tags-editor"

function at<T>(arr: T[], i: number): T {
  const v = arr[i]
  if (v === undefined) {
    throw new Error(`expected an element at index ${i}`)
  }
  return v
}

const INITIAL: Tag[] = [
  { Key: "env", Value: "prod" },
  { Key: "team", Value: "platform" },
]

describe("TagsEditor", () => {
  it("seeds rows from tags and disables Save with no changes", () => {
    render(<TagsEditor isPending={false} onSubmit={() => {}} tags={INITIAL} />)

    const keys = screen.getAllByLabelText("Tag key")
    expect(keys[0]).toHaveValue("env")
    expect(keys[1]).toHaveValue("team")
    expect(screen.getByText("No changes")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
  })

  it("submits the full desired tag set after an edit", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(<TagsEditor isPending={false} onSubmit={onSubmit} tags={INITIAL} />)

    const value = at(screen.getAllByLabelText("Tag value"), 0)
    await user.clear(value)
    await user.type(value, "staging")

    await user.click(screen.getByRole("button", { name: /save changes/i }))

    expect(onSubmit).toHaveBeenCalledWith([
      { key: "env", value: "staging" },
      { key: "team", value: "platform" },
    ])
  })

  it("adds and removes tag rows", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(<TagsEditor isPending={false} onSubmit={onSubmit} tags={INITIAL} />)

    await user.click(screen.getByRole("button", { name: /add tag/i }))
    await user.type(at(screen.getAllByLabelText("Tag key"), 2), "owner")
    await user.type(at(screen.getAllByLabelText("Tag value"), 2), "alice")

    await user.click(
      at(screen.getAllByRole("button", { name: /remove tag/i }), 1),
    )

    await user.click(screen.getByRole("button", { name: /save changes/i }))

    expect(onSubmit).toHaveBeenCalledWith([
      { key: "env", value: "prod" },
      { key: "owner", value: "alice" },
    ])
  })

  it("blocks submit on a duplicate key", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(<TagsEditor isPending={false} onSubmit={onSubmit} tags={INITIAL} />)

    const key = at(screen.getAllByLabelText("Tag key"), 1)
    await user.clear(key)
    await user.type(key, "env")

    expect(screen.getByText(/keys must be unique/i)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it("blocks submit on a value without a key", async () => {
    const user = userEvent.setup()
    const onSubmit = vi.fn()
    render(<TagsEditor isPending={false} onSubmit={onSubmit} tags={[]} />)

    await user.click(screen.getByRole("button", { name: /add tag/i }))
    await user.type(at(screen.getAllByLabelText("Tag value"), 0), "orphan")

    expect(screen.getByText(/needs a key/i)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /save changes/i })).toBeDisabled()
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it("Reset reverts edits back to initial", async () => {
    const user = userEvent.setup()
    render(<TagsEditor isPending={false} onSubmit={() => {}} tags={INITIAL} />)

    const value = at(screen.getAllByLabelText("Tag value"), 0)
    await user.clear(value)
    await user.type(value, "dev")
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: /reset/i }))

    expect(at(screen.getAllByLabelText("Tag value"), 0)).toHaveValue("prod")
    expect(screen.getByText("No changes")).toBeInTheDocument()
  })

  it("renders the Save button in pending state", () => {
    render(<TagsEditor isPending={true} onSubmit={() => {}} tags={INITIAL} />)
    expect(screen.getByRole("button", { name: /saving/i })).toBeDisabled()
  })

  it("renders an error banner when the mutation fails", () => {
    render(
      <TagsEditor
        error={new Error("boom")}
        isPending={false}
        onSubmit={() => {}}
        tags={INITIAL}
      />,
    )
    expect(screen.getByRole("alert")).toHaveTextContent("boom")
  })
})
