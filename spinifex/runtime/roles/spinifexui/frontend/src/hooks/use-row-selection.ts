import { useState } from "react"

export interface RowSelection {
  // The selected IDs still present in the list, in list order.
  ids: string[]
  selected: ReadonlySet<string>
  onSelectedChange: (next: Set<string>) => void
  clear: () => void
  // Drops the given IDs, such as the ones a bulk delete removed.
  drop: (ids: readonly string[]) => void
}

function keep(current: Set<string>, present: ReadonlySet<string>): Set<string> {
  const next = new Set([...current].filter((id) => present.has(id)))

  return next.size === current.size ? current : next
}

// Row selection for a list with a side panel. Selecting closes the panel,
// since the selection bar and the panel compete for the same attention. A row
// that leaves the list leaves the selection, so clearing a filter never
// brings back a selection the customer could not see.
export function useRowSelection(
  rowIds: readonly string[],
  panelOpen: boolean,
  closePanel: () => void,
): RowSelection {
  const [selected, setSelected] = useState<Set<string>>(() => new Set())
  // IDs are resource identifiers, which never contain a newline.
  const rowKey = rowIds.join("\n")
  const [seenKey, setSeenKey] = useState(rowKey)

  // When the list changes, rows that left it leave the stored selection too.
  // Guarded by the key, so it runs once per change and cannot loop.
  if (seenKey !== rowKey) {
    setSeenKey(rowKey)
    setSelected((current) => keep(current, new Set(rowIds)))
  }

  return {
    ids: rowIds.filter((id) => selected.has(id)),
    selected,
    onSelectedChange: (next) => {
      setSelected(next)

      if (next.size > 0 && panelOpen) {
        closePanel()
      }
    },
    clear: () => {
      setSelected(new Set())
    },
    drop: (ids) => {
      setSelected((current) => {
        const next = new Set(current)

        for (const id of ids) {
          next.delete(id)
        }

        return next
      })
    },
  }
}
