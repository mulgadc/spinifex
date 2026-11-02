import { act, render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { DetailPanel } from "./detail-panel"
import { InlineTags, validateTag } from "./inline-tags"

const TAGS = [
  { Key: "env", Value: "prod" },
  { Key: "team", Value: "web" },
]

function setup(
  overrides: {
    onPut?: (key: string, value: string) => Promise<void>
    onRemove?: (key: string) => Promise<void>
  } = {},
) {
  const onPut = vi.fn(overrides.onPut ?? (async () => {}))
  const onRemove = vi.fn(overrides.onRemove ?? (async () => {}))
  render(<InlineTags onPut={onPut} onRemove={onRemove} tags={TAGS} />)

  return { onPut, onRemove, user: userEvent.setup() }
}

describe("validateTag", () => {
  it("refuses what AWS refuses", () => {
    expect(validateTag("", "v", [])).toBe("Tag key is required")
    expect(validateTag("k".repeat(129), "", [])).toMatch(/128/u)
    expect(validateTag("k".repeat(128), "v".repeat(256), [])).toBeUndefined()
    expect(validateTag("k", "v".repeat(257), [])).toMatch(/256/u)
    expect(validateTag("AWS:name", "", [])).toMatch(/reserved/u)
    expect(validateTag("env", "", ["env"])).toMatch(/already exists/u)
  })
})

describe("InlineTags", () => {
  it("adds a tag with one put", async () => {
    const { onPut, onRemove, user } = setup()

    await user.click(screen.getByRole("button", { name: "Add tag" }))
    await user.type(screen.getByRole("textbox", { name: "Tag key" }), "owner")
    await user.type(
      screen.getByRole("textbox", { name: "Tag value" }),
      "ops{Enter}",
    )

    expect(onPut).toHaveBeenCalledExactlyOnceWith("owner", "ops")
    expect(onRemove).not.toHaveBeenCalled()
    await waitFor(() => {
      expect(screen.queryByRole("textbox", { name: "Tag key" })).toBeNull()
    })
  })

  it("edits a value without removing the key", async () => {
    const { onPut, onRemove, user } = setup()

    await user.click(screen.getByRole("button", { name: "Edit tag env" }))
    const value = screen.getByRole("textbox", { name: "Tag value" })
    await user.clear(value)
    await user.type(value, "staging")
    await user.click(screen.getByRole("button", { name: "Save tag env" }))

    expect(onPut).toHaveBeenCalledExactlyOnceWith("env", "staging")
    expect(onRemove).not.toHaveBeenCalled()
  })

  it("renames by putting the new key before removing the old", async () => {
    const calls: string[] = []

    const { user } = setup({
      onPut: async (key) => {
        calls.push(`put ${key}`)
      },
      onRemove: async (key) => {
        calls.push(`remove ${key}`)
      },
    })

    await user.click(screen.getByRole("button", { name: "Edit tag env" }))
    const key = screen.getByRole("textbox", { name: "Tag key" })
    await user.clear(key)
    await user.type(key, "stage{Enter}")

    await waitFor(() => {
      expect(calls).toStrictEqual(["put stage", "remove env"])
    })
  })

  it("keeps the row open and says what happened when a rename half fails", async () => {
    const { onPut, user } = setup({
      onRemove: async () => {
        throw new Error("AccessDenied")
      },
    })

    await user.click(screen.getByRole("button", { name: "Edit tag env" }))
    const key = screen.getByRole("textbox", { name: "Tag key" })
    await user.clear(key)
    await user.type(key, "stage{Enter}")

    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "Saved stage, but env could not be removed: AccessDenied",
    )
    expect(onPut).toHaveBeenCalledExactlyOnceWith("stage", "prod")
    expect(screen.getByRole("textbox", { name: "Tag key" })).toHaveValue(
      "stage",
    )
  })

  it("removes a tag", async () => {
    const { onPut, onRemove, user } = setup()

    await user.click(screen.getByRole("button", { name: "Remove tag team" }))

    expect(onRemove).toHaveBeenCalledExactlyOnceWith("team")
    expect(onPut).not.toHaveBeenCalled()
  })

  it("shows a failed remove beside the row", async () => {
    const { user } = setup({
      onRemove: async () => {
        throw new Error("Throttling")
      },
    })

    await user.click(screen.getByRole("button", { name: "Remove tag team" }))

    await expect(screen.findByRole("alert")).resolves.toHaveTextContent(
      "Throttling",
    )
  })

  it("refuses an invalid tag without calling the API", async () => {
    const { onPut, user } = setup()

    await user.click(screen.getByRole("button", { name: "Add tag" }))
    await user.type(
      screen.getByRole("textbox", { name: "Tag key" }),
      "aws:name{Enter}",
    )
    expect(screen.getByRole("alert")).toHaveTextContent(/reserved/u)

    const key = screen.getByRole("textbox", { name: "Tag key" })
    await user.clear(key)
    await user.type(key, "team{Enter}")
    expect(screen.getByRole("alert")).toHaveTextContent(/already exists/u)

    await user.clear(key)
    await user.keyboard("{Enter}")
    expect(screen.getByRole("alert")).toHaveTextContent("Tag key is required")

    expect(onPut).not.toHaveBeenCalled()
  })

  it("cancels an edit on Escape without closing the panel", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const onClose = vi.fn()
    const onPut = vi.fn(async () => {})
    render(
      <DetailPanel
        closeLabel="Close"
        label="Volume details"
        onClose={onClose}
        title="vol-1"
      >
        <InlineTags onPut={onPut} onRemove={vi.fn()} tags={TAGS} />
      </DetailPanel>,
    )
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })

    await user.click(screen.getByRole("button", { name: "Edit tag env" }))
    await user.type(screen.getByRole("textbox", { name: "Tag value" }), "x")
    await user.keyboard("{Escape}")
    act(() => {
      vi.advanceTimersByTime(500)
    })

    expect(screen.queryByRole("textbox", { name: "Tag value" })).toBeNull()
    expect(screen.getByText("prod")).toBeInTheDocument()
    expect(onPut).not.toHaveBeenCalled()
    expect(onClose).not.toHaveBeenCalled()

    // With no row editing, the same key closes the panel.
    await user.keyboard("{Escape}")
    act(() => {
      vi.advanceTimersByTime(500)
    })
    expect(onClose).toHaveBeenCalledOnce()
    vi.useRealTimers()
  })

  it("is read-only without callbacks", () => {
    render(<InlineTags tags={TAGS} />)

    expect(screen.getByText("env")).toBeInTheDocument()
    expect(screen.getByText("prod")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /tag/u })).toBeNull()
  })

  it("says when there are no tags", () => {
    render(<InlineTags tags={[]} />)

    expect(screen.getByText("No tags.")).toBeInTheDocument()
  })
})
