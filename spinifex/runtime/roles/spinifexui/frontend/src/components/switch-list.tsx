import { useId, type ReactNode } from "react"

import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"

export interface SwitchListItem {
  id: string
  label: string
  // A second line, for what tells two similar rows apart (an AZ, a CIDR).
  detail?: ReactNode
}

interface SwitchListProps {
  items: SwitchListItem[]
  selected: string[]
  onChange: (next: string[]) => void
  emptyText: string
  // Prefixes each row's accessible name, as in "Subnet subnet-0abc".
  itemLabel: string
  mono?: boolean
}

// A boxed multi-select, one switch per row, for picking subnets, security
// groups and the like. The form's switches and this list share one control.
export function SwitchList({
  items,
  selected,
  onChange,
  emptyText,
  itemLabel,
  mono = true,
}: SwitchListProps) {
  const baseId = useId()

  if (items.length === 0) {
    return <p className="text-xs text-muted-foreground">{emptyText}</p>
  }

  const chosen = new Set(selected)

  return (
    <ul className="divide-y rounded-md border bg-background/40">
      {items.map((item, index) => {
        const id = `${baseId}-${index}`

        return (
          <li
            className="flex items-center justify-between gap-3 px-3 py-2"
            key={item.id}
          >
            <Label className="min-w-0 flex-1" htmlFor={id}>
              <span className="flex min-w-0 flex-col gap-1">
                <span
                  className={mono ? "truncate font-mono text-xs" : "truncate"}
                >
                  {item.label}
                </span>
                {item.detail && (
                  <span className="text-xs font-normal text-muted-foreground">
                    {item.detail}
                  </span>
                )}
              </span>
            </Label>
            <Switch
              aria-label={`${itemLabel} ${item.label}`}
              checked={chosen.has(item.id)}
              id={id}
              onCheckedChange={(checked) => {
                onChange(
                  checked
                    ? [...selected, item.id]
                    : selected.filter((value) => value !== item.id),
                )
              }}
            />
          </li>
        )
      })}
    </ul>
  )
}
