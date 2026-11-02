import { Plus, Trash2 } from "lucide-react"
import { type ClipboardEvent, type KeyboardEvent, useRef } from "react"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"

// One single-line input per value of the record set. The form still holds the
// values as newline-joined text, so the schema and the API mapping are as before.
export function RecordValuesField({
  id,
  label,
  value,
  onChange,
  multiple,
  invalid,
}: {
  id: string
  label: string
  value: string
  onChange: (next: string) => void
  multiple: boolean
  invalid: boolean
}) {
  const lines = value === "" ? [""] : value.split("\n")
  const inputs = useRef<(HTMLInputElement | null)[]>([])

  const commit = (next: string[], focus?: number) => {
    onChange(next.join("\n"))

    if (focus !== undefined) {
      requestAnimationFrame(() => inputs.current[focus]?.focus())
    }
  }

  const setLine = (index: number, text: string) => {
    commit(lines.map((line, i) => (i === index ? text : line)))
  }

  const addAfter = (index: number) => {
    commit(
      [...lines.slice(0, index + 1), "", ...lines.slice(index + 1)],
      index + 1,
    )
  }

  const remove = (index: number) => {
    commit(
      lines.filter((_, i) => i !== index),
      Math.max(0, index - 1),
    )
  }

  const onKeyDown = (index: number, event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key !== "Enter") {
      return
    }

    // Enter would submit the form; with several values allowed it starts the next one.
    event.preventDefault()

    if (multiple) {
      addAfter(index)
    }
  }

  // A pasted list becomes one row per line rather than one value with breaks in it.
  const onPaste = (index: number, event: ClipboardEvent<HTMLInputElement>) => {
    const text = event.clipboardData.getData("text")

    if (!text.includes("\n")) {
      return
    }

    event.preventDefault()

    const pasted = text
      .split(/\r?\n/)
      .map((l) => l.trim())
      .filter(Boolean)

    const rows = multiple ? pasted : pasted.slice(0, 1)
    const next = [...lines.slice(0, index), ...rows, ...lines.slice(index + 1)]
    commit(next, index + rows.length - 1)
  }

  return (
    <div className="space-y-2">
      {lines.map((line, index) => (
        // oxlint-disable-next-line no-array-index-key -- a value row has no identity beyond its position
        <div className="flex items-center gap-2" key={index}>
          <Input
            aria-invalid={invalid}
            aria-label={index === 0 ? undefined : `${label} ${index + 1}`}
            autoComplete="off"
            variant="code"
            id={index === 0 ? id : undefined}
            onChange={(e) => {
              setLine(index, e.target.value)
            }}
            onKeyDown={(e) => {
              onKeyDown(index, e)
            }}
            onPaste={(e) => {
              onPaste(index, e)
            }}
            ref={(el) => {
              inputs.current[index] = el
            }}
            spellCheck={false}
            value={line}
          />
          {lines.length > 1 && (
            <Button
              aria-label={`Remove ${label.toLowerCase()} ${index + 1}`}
              onClick={() => {
                remove(index)
              }}
              size="icon-sm"
              type="button"
              variant="ghost"
            >
              <Trash2 />
            </Button>
          )}
        </div>
      ))}
      {multiple && (
        <Button
          onClick={() => {
            addAfter(lines.length - 1)
          }}
          size="sm"
          type="button"
          variant="outline"
        >
          <Plus data-icon="inline-start" />
          Add value
        </Button>
      )}
    </div>
  )
}
