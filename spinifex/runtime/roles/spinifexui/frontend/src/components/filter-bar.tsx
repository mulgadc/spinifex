import { Search } from "lucide-react"

import { Button } from "@/components/ui/button"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
} from "@/components/ui/input-group"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

export interface FilterChoice<T extends string> {
  value: T
  label: string
  // How many rows the choice keeps, shown beside its label.
  count?: number
}

interface FilterBarProps<T extends string> {
  query: string
  onQueryChange: (query: string) => void
  // Names the search box for assistive tech, as in "Filter instances".
  searchLabel: string
  placeholder: string
  // The first choice is the unfiltered one that Clear filters returns to.
  choices?: readonly FilterChoice<T>[]
  choice?: T
  onChoiceChange?: (choice: T) => void
  choicesLabel?: string
  // Clears both in one step, for a list that keeps its filter in the URL.
  onClear?: () => void
}

const NO_CHOICES: readonly never[] = []

// A list's search box, one-of filter and Clear filters, laid out as the
// instances list has them so every list filters the same way.
export function FilterBar<T extends string>({
  query,
  onQueryChange,
  searchLabel,
  placeholder,
  choices = NO_CHOICES,
  choice,
  onChoiceChange,
  choicesLabel,
  onClear,
}: FilterBarProps<T>) {
  const unfiltered = choices[0]?.value

  const filtered =
    query !== "" || (choice !== undefined && choice !== unfiltered)

  return (
    <div className="flex flex-wrap items-center gap-2">
      <InputGroup className="w-full sm:w-72">
        <InputGroupAddon>
          <Search />
        </InputGroupAddon>
        <InputGroupInput
          aria-label={searchLabel}
          onChange={(event) => {
            onQueryChange(event.target.value)
          }}
          placeholder={placeholder}
          type="search"
          value={query}
        />
      </InputGroup>
      {choices.length > 0 && choice !== undefined && (
        <ToggleGroup
          aria-label={choicesLabel}
          onValueChange={(values) => {
            // A click on the pressed item empties the group; keep the choice.
            const next = choices.find((item) => item.value === values.at(-1))

            if (next) {
              onChoiceChange?.(next.value)
            }
          }}
          value={[choice]}
          variant="outline"
        >
          {choices.map((item) => (
            <ToggleGroupItem key={item.value} value={item.value}>
              <span>{item.label}</span>
              {item.count !== undefined && (
                <span className="text-muted-foreground tabular-nums">
                  {item.count}
                </span>
              )}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      )}
      {filtered && (
        <Button
          onClick={() => {
            if (onClear) {
              onClear()

              return
            }

            onQueryChange("")

            if (unfiltered !== undefined) {
              onChoiceChange?.(unfiltered)
            }
          }}
          size="sm"
          variant="ghost"
        >
          Clear filters
        </Button>
      )}
    </div>
  )
}
