import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { type DataColumn, DataTable } from "./data-table"

interface Row {
  id: string
  ip?: string
}

const COLUMNS: DataColumn<Row>[] = [
  { id: "id", header: "ID", cell: (row) => row.id },
  {
    id: "ip",
    header: "IP",
    cell: (row) => row.ip,
    sortValue: (row) => row.ip,
  },
]

const ROWS: Row[] = [
  { id: "a", ip: "10.0.0.10" },
  { id: "b" },
  { id: "c", ip: "10.0.0.9" },
]

function renderedIds(): string[] {
  return screen
    .getAllByRole("row")
    .slice(1)
    .map((row) => row.querySelector("td")?.textContent ?? "")
}

describe("DataTable", () => {
  it("orders addresses numerically, not as text", () => {
    render(
      <DataTable
        caption="Rows"
        columns={COLUMNS}
        empty="None"
        getRowId={(row) => row.id}
        rows={ROWS}
        sort={{ column: "ip", desc: false }}
      />,
    )

    expect(renderedIds()).toStrictEqual(["c", "a", "b"])
  })

  it("keeps missing values last when descending too", () => {
    render(
      <DataTable
        caption="Rows"
        columns={COLUMNS}
        empty="None"
        getRowId={(row) => row.id}
        rows={ROWS}
        sort={{ column: "ip", desc: true }}
      />,
    )

    expect(renderedIds()).toStrictEqual(["a", "c", "b"])
  })

  it("leaves rows in their given order without a sort", () => {
    render(
      <DataTable
        caption="Rows"
        columns={COLUMNS}
        empty="None"
        getRowId={(row) => row.id}
        rows={ROWS}
      />,
    )

    expect(renderedIds()).toStrictEqual(["a", "b", "c"])
  })

  it("opens a row on click but not from its checkbox", async () => {
    const onRowClick = vi.fn()

    render(
      <DataTable
        activeId="c"
        caption="Rows"
        columns={COLUMNS}
        empty="None"
        getRowId={(row) => row.id}
        onRowClick={onRowClick}
        onSelectedChange={vi.fn()}
        rows={ROWS}
        selected={new Set()}
      />,
    )

    await userEvent.click(screen.getByText("10.0.0.10"))
    await userEvent.click(screen.getByRole("checkbox", { name: "Select b" }))
    expect(onRowClick).toHaveBeenCalledExactlyOnceWith("a")

    expect(screen.getByText("10.0.0.9").closest("tr")).toHaveAttribute(
      "data-active",
    )
  })

  it("cycles a sortable header ascending then descending", async () => {
    const onSortChange = vi.fn()

    const { rerender } = render(
      <DataTable
        caption="Rows"
        columns={COLUMNS}
        empty="None"
        getRowId={(row) => row.id}
        onSortChange={onSortChange}
        rows={ROWS}
      />,
    )

    await userEvent.click(screen.getByRole("button", { name: "IP" }))
    expect(onSortChange).toHaveBeenLastCalledWith({ column: "ip", desc: false })

    rerender(
      <DataTable
        caption="Rows"
        columns={COLUMNS}
        empty="None"
        getRowId={(row) => row.id}
        onSortChange={onSortChange}
        rows={ROWS}
        sort={{ column: "ip", desc: false }}
      />,
    )

    expect(screen.getByRole("columnheader", { name: "IP" })).toHaveAttribute(
      "aria-sort",
      "ascending",
    )
    await userEvent.click(screen.getByRole("button", { name: "IP" }))
    expect(onSortChange).toHaveBeenLastCalledWith({ column: "ip", desc: true })
  })

  it("selects every row from the header and clears them again", async () => {
    const onSelectedChange = vi.fn()

    const { rerender } = render(
      <DataTable
        caption="Rows"
        columns={COLUMNS}
        empty="None"
        getRowId={(row) => row.id}
        onSelectedChange={onSelectedChange}
        rows={ROWS}
        selected={new Set(["a"])}
      />,
    )

    await userEvent.click(
      screen.getByRole("checkbox", { name: "Select all rows" }),
    )
    expect(onSelectedChange).toHaveBeenLastCalledWith(new Set(["a", "b", "c"]))

    rerender(
      <DataTable
        caption="Rows"
        columns={COLUMNS}
        empty="None"
        getRowId={(row) => row.id}
        onSelectedChange={onSelectedChange}
        rows={ROWS}
        selected={new Set(["a", "b", "c"])}
      />,
    )

    await userEvent.click(
      screen.getByRole("checkbox", { name: "Select all rows" }),
    )
    expect(onSelectedChange).toHaveBeenLastCalledWith(new Set())
  })

  it("spans the empty message across every column", () => {
    render(
      <DataTable
        caption="Rows"
        columns={COLUMNS}
        empty="Nothing here"
        getRowId={(row) => row.id}
        onSelectedChange={vi.fn()}
        rows={[]}
        selected={new Set()}
      />,
    )

    expect(screen.getByText("Nothing here").closest("td")).toHaveAttribute(
      "colspan",
      "3",
    )
  })
})
