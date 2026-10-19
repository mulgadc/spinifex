import { Fragment } from "react"
import type { ReactNode } from "react"

interface ResourceListProps<T> {
  items: readonly T[]
  empty: string
  getKey: (item: T) => string | undefined
  renderItem: (item: T, key: string) => ReactNode
}

// Items whose key is missing are skipped; the narrowed key is passed to
// renderItem so callers can use it in route params without re-checking.
export function ResourceList<T>({
  items,
  empty,
  getKey,
  renderItem,
}: ResourceListProps<T>) {
  if (items.length === 0) {
    return <p className="text-muted-foreground">{empty}</p>
  }

  return (
    <div className="space-y-4">
      {items.map((item) => {
        const key = getKey(item)

        if (!key) {
          return null
        }

        return <Fragment key={key}>{renderItem(item, key)}</Fragment>
      })}
    </div>
  )
}
