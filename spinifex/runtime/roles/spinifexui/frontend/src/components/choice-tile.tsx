import { Check } from "lucide-react"
import { createContext, type ReactNode, use, useId } from "react"

const COLUMNS = {
  2: "grid-cols-2",
  3: "grid-cols-2 sm:grid-cols-3",
  4: "grid-cols-2 sm:grid-cols-4",
} as const

// One radio name per group, so the browser moves between tiles with arrows.
const GroupName = createContext<string | undefined>(undefined)

interface ChoiceTilesProps {
  label: string
  children: ReactNode
  // Tiles per row from the sm breakpoint; two per row below it.
  columns?: keyof typeof COLUMNS
}

export function ChoiceTiles({
  label,
  children,
  columns = 4,
}: ChoiceTilesProps) {
  const name = useId()

  return (
    <fieldset className={`grid min-w-0 gap-2 ${COLUMNS[columns]}`}>
      <legend className="sr-only">{label}</legend>
      <GroupName value={name}>{children}</GroupName>
    </fieldset>
  )
}

interface ChoiceTileProps {
  label: string
  checked: boolean
  // Absent for a read-only tile, which shows the one choice there is.
  onSelect?: () => void
  icon: ReactNode
  description?: ReactNode
  layout?: "stacked" | "inline"
}

// A selectable card over a native radio, so the group keeps arrow-key
// movement and the checked state screen readers expect.
export function ChoiceTile({
  label,
  checked,
  onSelect,
  icon,
  description,
  layout = "stacked",
}: ChoiceTileProps) {
  const id = useId()
  const name = use(GroupName)

  return (
    <label
      className="group relative flex min-w-0 cursor-pointer items-center gap-2.5 rounded-lg border bg-background/40 p-3 text-left text-sm transition-colors hover:bg-muted/60 has-checked:border-primary has-checked:bg-primary/5 has-focus-visible:ring-2 has-focus-visible:ring-ring/40 has-disabled:cursor-default has-disabled:opacity-100 has-disabled:hover:bg-background/40 has-checked:has-disabled:hover:bg-primary/5 data-stacked:flex-col data-stacked:justify-center data-stacked:text-center"
      data-stacked={layout === "stacked" ? "" : undefined}
      htmlFor={id}
    >
      <input
        aria-label={label}
        checked={checked}
        className="sr-only"
        disabled={!onSelect}
        id={id}
        name={name}
        onChange={onSelect}
        type="radio"
      />
      <span className="flex shrink-0 items-center justify-center">{icon}</span>
      <span className="flex min-w-0 flex-col">
        <span className="truncate font-medium">{label}</span>
        {description && (
          <span className="truncate text-xs text-muted-foreground">
            {description}
          </span>
        )}
      </span>
      {checked && (
        <Check
          aria-hidden
          className="absolute top-1.5 right-1.5 size-3.5 text-primary"
        />
      )}
    </label>
  )
}
