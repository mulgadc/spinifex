import { render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { BulkDelete } from "./bulk-delete"

function setup(deleteOne: (id: string) => Promise<object>) {
  const onDeleted = vi.fn()

  render(
    <BulkDelete
      deleteOne={deleteOne}
      ids={["vol-1", "vol-2", "vol-3"]}
      label={(id) => `disk ${id}`}
      noun={["volume", "volumes"]}
      onDeleted={onDeleted}
    />,
  )

  return { onDeleted, user: userEvent.setup() }
}

describe("BulkDelete", () => {
  it("asks first, naming every selected resource", async () => {
    const deleteOne = vi.fn(async (_id: string) => ({}))
    const { user } = setup(deleteOne)

    await user.click(screen.getByRole("button", { name: "Delete" }))

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("Delete 3 volumes?")).toBeInTheDocument()
    expect(within(dialog).getByText("disk vol-2")).toBeInTheDocument()
    expect(deleteOne).not.toHaveBeenCalled()
  })

  it("deletes one at a time and closes when all succeed", async () => {
    const deleteOne = vi.fn(async (_id: string) => ({}))
    const { onDeleted, user } = setup(deleteOne)

    await user.click(screen.getByRole("button", { name: "Delete" }))
    await user.click(screen.getByRole("button", { name: "Delete 3" }))

    expect(deleteOne.mock.calls.map(([id]) => id)).toStrictEqual([
      "vol-1",
      "vol-2",
      "vol-3",
    ])
    expect(onDeleted).toHaveBeenCalledWith(["vol-1", "vol-2", "vol-3"])
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
  })

  it("keeps each refusal against its resource and retries only those", async () => {
    const deleteOne = vi.fn(async (id: string) => {
      if (id === "vol-2") {
        throw new Error("VolumeInUse: vol-2 is attached")
      }

      return {}
    })

    const { onDeleted, user } = setup(deleteOne)

    await user.click(screen.getByRole("button", { name: "Delete" }))
    await user.click(screen.getByRole("button", { name: "Delete 3" }))

    const dialog = screen.getByRole("alertdialog")

    expect(onDeleted).toHaveBeenLastCalledWith(["vol-1", "vol-3"])
    expect(
      within(dialog).getByText("1 could not be deleted"),
    ).toBeInTheDocument()
    expect(
      within(dialog).getByText("VolumeInUse: vol-2 is attached"),
    ).toBeInTheDocument()

    deleteOne.mockClear()
    await user.click(within(dialog).getByRole("button", { name: "Retry 1" }))

    expect(deleteOne.mock.calls.map(([id]) => id)).toStrictEqual(["vol-2"])
  })

  it("asks for the typed phrase where one is set", async () => {
    const deleteOne = vi.fn(async (_id: string) => ({}))
    const user = userEvent.setup()

    render(
      <BulkDelete
        confirmPhrase="delete"
        deleteOne={deleteOne}
        ids={["vol-1", "vol-2"]}
        noun={["volume", "volumes"]}
        onDeleted={vi.fn()}
      />,
    )

    await user.click(screen.getByRole("button", { name: "Delete" }))

    const dialog = screen.getByRole("alertdialog")
    const confirm = within(dialog).getByRole("button", { name: "Delete 2" })

    expect(confirm).toBeDisabled()

    await user.type(
      within(dialog).getByLabelText("To confirm, type “delete” in this field."),
      "DELETE",
    )
    await user.click(confirm)

    expect(deleteOne).toHaveBeenCalledTimes(2)
  })

  it("hands a batch deleter every ID in one call, keeping its refusals", async () => {
    const deleteMany = vi.fn(
      async (_ids: readonly string[]) => new Map([["b", "Access Denied"]]),
    )

    const onDeleted = vi.fn()
    const user = userEvent.setup()

    render(
      <BulkDelete
        deleteMany={deleteMany}
        ids={["a", "b"]}
        noun={["item", "items"]}
        onDeleted={onDeleted}
      />,
    )

    await user.click(screen.getByRole("button", { name: "Delete" }))
    await user.click(screen.getByRole("button", { name: "Delete 2" }))

    expect(deleteMany).toHaveBeenCalledExactlyOnceWith(["a", "b"])
    expect(onDeleted).toHaveBeenLastCalledWith(["a"])
    expect(
      within(screen.getByRole("alertdialog")).getByText("Access Denied"),
    ).toBeInTheDocument()
  })
})
