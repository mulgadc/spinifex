import { useState } from "react"

interface RowSelection {
  // The selected IDs still present in the list, in list order.
  ids: string[]
  selected: ReadonlySet<string>
  onSelectedChange: (next: Set<string>) => void
  clear: () => void
  // Drops the given IDs, such as the ones a bulk delete removed.
  drop: (ids: readonly string[]) => void
}

// Row selection for a list with a side panel. Selecting closes the panel,
// since the selection bar and the panel compete for the same attention.
export function useRowSelection(
  rowIds: readonly string[],
  panelOpen: boolean,
  closePanel: () => void,
): RowSelection {
  const [selected, setSelected] = useState<Set<string>>(() => new Set())

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
