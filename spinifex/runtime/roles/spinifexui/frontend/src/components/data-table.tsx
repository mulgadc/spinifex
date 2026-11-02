import {
  type ColumnDef,
  createSortedRowModel,
  type Row,
  type RowData,
  type RowSelectionState,
  rowSelectionFeature,
  rowSortingFeature,
  type SortFn,
  type SortingState,
  tableFeatures,
  type Updater,
  functionalUpdate,
  useTable,
} from "@tanstack/react-table"
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react"
import {
  type MouseEvent as ReactMouseEvent,
  type ReactNode,
  useMemo,
} from "react"

import { Checkbox } from "@/components/ui/checkbox"
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"

export type Breakpoint = "sm" | "md" | "lg" | "xl"

export interface DataColumn<T> {
  id: string
  header: string
  cell: (row: T) => ReactNode
  // Present only on sortable columns. Missing values sort last either way.
  sortValue?: (row: T) => string | number | undefined
  // The column drops out when the table is narrower than this, so a page
  // beside an open detail panel sheds columns the way a phone does.
  hideBelow?: Breakpoint
  align?: "end"
}

export interface SortState {
  column: string
  desc: boolean
}

interface DataTableProps<T> {
  caption: string
  columns: readonly DataColumn<T>[]
  rows: readonly T[]
  getRowId: (row: T) => string
  empty: ReactNode
  sort?: SortState
  onSortChange?: (sort: SortState) => void
  selected?: ReadonlySet<string>
  onSelectedChange?: (selected: Set<string>) => void
  // The row whose detail panel is open, and what a click on a row opens.
  activeId?: string
  onRowClick?: (id: string) => void
}

const features = tableFeatures({
  rowSortingFeature,
  rowSelectionFeature,
  sortedRowModel: createSortedRowModel(),
})

type Features = typeof features

// Numeric collation puts 10.0.0.9 before 10.0.0.10, and keeps millisecond
// timestamps, which share a digit count, in time order.
const collator = new Intl.Collator(undefined, { numeric: true })

function collate<T extends RowData>(
  a: Row<Features, T>,
  b: Row<Features, T>,
  columnId: string,
): number {
  return collator.compare(
    String(a.getValue(columnId)),
    String(b.getValue(columnId)),
  )
}

function toColumnDef<T extends RowData>(
  column: DataColumn<T>,
): ColumnDef<Features, T> {
  const sortFn: SortFn<Features, T> = collate

  return {
    id: column.id,
    header: column.header,
    accessorFn: (row) => column.sortValue?.(row),
    cell: ({ row }): ReactNode => column.cell(row.original),
    enableSorting: column.sortValue !== undefined,
    sortFn,
    sortUndefined: "last",
  }
}

function SortIcon({ direction }: { direction: false | "asc" | "desc" }) {
  if (direction === "asc") {
    return <ArrowUp aria-hidden className="size-3" />
  }

  if (direction === "desc") {
    return <ArrowDown aria-hidden className="size-3" />
  }

  return <ChevronsUpDown aria-hidden className="size-3 opacity-50" />
}

const ARIA_SORT = { asc: "ascending", desc: "descending" } as const

// Clicks on a control inside the row belong to that control, not to the row.
function fromControl(event: ReactMouseEvent): boolean {
  return (
    event.target instanceof Element &&
    event.target.closest("a, button, input, label, [role=checkbox]") !== null
  )
}

// TanStack Table over the ui/table primitives. Sorting and selection stay
// controlled by the caller, so a page can keep both in its URL.
export function DataTable<T extends RowData>({
  caption,
  columns,
  rows,
  getRowId,
  empty,
  sort,
  onSortChange,
  selected,
  onSelectedChange,
  activeId,
  onRowClick,
}: DataTableProps<T>) {
  const selectable = selected !== undefined && onSelectedChange !== undefined

  const columnDefs = useMemo(() => columns.map(toColumnDef), [columns])

  const byId = useMemo(
    () => new Map(columns.map((column) => [column.id, column])),
    [columns],
  )

  const sorting: SortingState = sort
    ? [{ id: sort.column, desc: sort.desc }]
    : []

  const rowSelection: RowSelectionState = Object.fromEntries(
    [...(selected ?? [])].map((id) => [id, true]),
  )

  const table = useTable({
    features,
    columns: columnDefs,
    data: rows,
    getRowId,
    enableSortingRemoval: false,
    enableRowSelection: selectable,
    state: { sorting, rowSelection },
    onSortingChange: (updater: Updater<SortingState>) => {
      const [next] = functionalUpdate(updater, sorting)

      if (next) {
        onSortChange?.({ column: next.id, desc: next.desc })
      }
    },
    onRowSelectionChange: (updater: Updater<RowSelectionState>) => {
      const next = functionalUpdate(updater, rowSelection)

      onSelectedChange?.(
        new Set(Object.keys(next).filter((id) => next[id] === true)),
      )
    },
  })

  const visibleRows = table.getRowModel().rows

  return (
    <div className="overflow-hidden rounded-lg border bg-card">
      <Table>
        <TableCaption className="sr-only">{caption}</TableCaption>
        <TableHeader>
          {table.getHeaderGroups().map((group) => (
            <TableRow key={group.id}>
              {selectable && (
                <TableHead>
                  <Checkbox
                    aria-label="Select all rows"
                    checked={table.getIsAllRowsSelected()}
                    disabled={visibleRows.length === 0}
                    indeterminate={table.getIsSomeRowsSelected()}
                    onCheckedChange={(on) => {
                      table.toggleAllRowsSelected(on)
                    }}
                  />
                </TableHead>
              )}
              {group.headers.map((header) => {
                const column = byId.get(header.column.id)
                const direction = header.column.getIsSorted()

                return (
                  <TableHead
                    aria-sort={direction ? ARIA_SORT[direction] : undefined}
                    data-align={column?.align}
                    data-hide-below={column?.hideBelow}
                    key={header.id}
                  >
                    {header.column.getCanSort() && onSortChange ? (
                      <button
                        className="-mx-1 inline-flex items-center gap-1 rounded-sm px-1 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                        onClick={header.column.getToggleSortingHandler()}
                        type="button"
                      >
                        <table.FlexRender header={header} />
                        <SortIcon direction={direction} />
                      </button>
                    ) : (
                      <table.FlexRender header={header} />
                    )}
                  </TableHead>
                )
              })}
            </TableRow>
          ))}
        </TableHeader>
        <TableBody>
          {visibleRows.length === 0 && (
            <TableRow>
              <TableCell colSpan={columns.length + Number(selectable)}>
                <div className="flex min-h-32 items-center justify-center text-center whitespace-normal text-muted-foreground">
                  {empty}
                </div>
              </TableCell>
            </TableRow>
          )}
          {visibleRows.map((row) => {
            const toggle = row.getToggleSelectedHandler()

            return (
              <TableRow
                data-active={row.id === activeId ? "" : undefined}
                data-clickable={onRowClick ? "" : undefined}
                data-state={row.getIsSelected() ? "selected" : undefined}
                key={row.id}
                onClick={(event) => {
                  if (onRowClick && !fromControl(event)) {
                    onRowClick(row.id)
                  }
                }}
              >
                {selectable && (
                  <TableCell>
                    <Checkbox
                      aria-label={`Select ${row.id}`}
                      checked={row.getIsSelected()}
                      onCheckedChange={(on, details) => {
                        // The handler reads target.checked, and Shift from the
                        // click, to select a range from the last row toggled.
                        toggle({
                          target: { checked: on },
                          shiftKey:
                            details.event instanceof MouseEvent &&
                            details.event.shiftKey,
                        })
                      }}
                    />
                  </TableCell>
                )}
                {row.getAllCells().map((cell) => {
                  const column = byId.get(cell.column.id)

                  return (
                    <TableCell
                      data-align={column?.align}
                      data-hide-below={column?.hideBelow}
                      key={cell.id}
                    >
                      <table.FlexRender cell={cell} />
                    </TableCell>
                  )
                })}
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </div>
  )
}
