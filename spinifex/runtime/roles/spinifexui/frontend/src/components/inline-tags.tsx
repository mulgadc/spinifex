import { Check, LoaderCircle, Pencil, Plus, Trash2, X } from "lucide-react"
import { type KeyboardEvent, useId, useState } from "react"

import { PanelSection } from "@/components/detail-panel"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"

interface Tag {
  Key?: string
  Value?: string
}

interface InlineTagsProps {
  tags: readonly Tag[] | undefined
  // Both absent means the tags cannot be changed, and the rows are read-only.
  onPut?: (key: string, value: string) => Promise<void>
  onRemove?: (key: string) => Promise<void>
}

const KEY_MAX = 128
const VALUE_MAX = 256

export function validateTag(
  key: string,
  value: string,
  otherKeys: readonly string[],
): string | undefined {
  if (key.length === 0) {
    return "Tag key is required"
  }

  if (key.length > KEY_MAX) {
    return `Tag key must be ${KEY_MAX} characters or less`
  }

  if (value.length > VALUE_MAX) {
    return `Tag value must be ${VALUE_MAX} characters or less`
  }

  if (key.toLowerCase().startsWith("aws:")) {
    return "Keys starting aws: are reserved"
  }

  return otherKeys.includes(key)
    ? `A tag with key ${key} already exists`
    : undefined
}

interface TagRowProps {
  // Absent for a row being added.
  original?: { key: string; value: string }
  otherKeys: readonly string[]
  editable: boolean
  onPut?: (key: string, value: string) => Promise<void>
  onRemove?: (key: string) => Promise<void>
  onDone?: () => void
}

function TagRow({
  original,
  otherKeys,
  editable,
  onPut,
  onRemove,
  onDone,
}: TagRowProps) {
  const id = useId()
  const [editing, setEditing] = useState(original === undefined)
  const [key, setKey] = useState(original?.key ?? "")
  const [value, setValue] = useState(original?.value ?? "")
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const name = original?.key ?? "new tag"

  function cancel() {
    setKey(original?.key ?? "")
    setValue(original?.value ?? "")
    setError(undefined)
    setEditing(false)
    onDone?.()
  }

  async function save() {
    const invalid = validateTag(key, value, otherKeys)

    if (invalid) {
      setError(invalid)

      return
    }

    if (!onPut || !onRemove) {
      return
    }

    setPending(true)
    setError(undefined)

    try {
      await onPut(key, value)
    } catch (error_) {
      setError(error_ instanceof Error ? error_.message : "The request failed")
      setPending(false)

      return
    }

    // AWS has no rename: the new key is written first, so a failure here leaves
    // both keys rather than neither.
    if (original && original.key !== key) {
      try {
        await onRemove(original.key)
      } catch (error_) {
        setError(
          `Saved ${key}, but ${original.key} could not be removed: ${error_ instanceof Error ? error_.message : "The request failed"}`,
        )
        setPending(false)

        return
      }
    }

    setPending(false)
    setEditing(false)
    onDone?.()
  }

  async function remove() {
    if (!original || !onRemove) {
      return
    }

    setPending(true)
    setError(undefined)

    try {
      await onRemove(original.key)
    } catch (error_) {
      setError(error_ instanceof Error ? error_.message : "The request failed")
      setPending(false)
    }
  }

  // Escape stays with the row, so cancelling an edit never closes the panel.
  function onKeyDown(event: KeyboardEvent) {
    if (event.key === "Escape") {
      event.stopPropagation()
      cancel()
    } else if (event.key === "Enter") {
      event.preventDefault()
      void save()
    }
  }

  const errorText = error && (
    <p className="col-span-full text-xs text-danger-foreground" role="alert">
      {error}
    </p>
  )

  if (editing) {
    return (
      <div className="grid grid-cols-[1fr_1fr_auto] items-center gap-1.5 py-1.5">
        <Input
          aria-invalid={error !== undefined}
          aria-label="Tag key"
          autoFocus
          disabled={pending}
          id={`${id}-key`}
          onChange={(event) => {
            setKey(event.target.value)
          }}
          onKeyDown={onKeyDown}
          placeholder="Key"
          value={key}
        />
        <Input
          aria-label="Tag value"
          disabled={pending}
          id={`${id}-value`}
          onChange={(event) => {
            setValue(event.target.value)
          }}
          onKeyDown={onKeyDown}
          placeholder="Value"
          value={value}
        />
        <div className="flex items-center gap-0.5">
          <Button
            aria-label={`Save tag ${name}`}
            disabled={pending}
            onClick={() => {
              void save()
            }}
            size="icon-sm"
            variant="ghost"
          >
            {pending ? (
              <LoaderCircle className="animate-spin motion-reduce:animate-none" />
            ) : (
              <Check />
            )}
          </Button>
          <Button
            aria-label={`Cancel editing ${name}`}
            disabled={pending}
            onClick={cancel}
            size="icon-sm"
            variant="ghost"
          >
            <X />
          </Button>
        </div>
        {errorText}
      </div>
    )
  }

  return (
    <div className="grid grid-cols-[minmax(0,2fr)_minmax(0,3fr)_auto] items-center gap-2 py-1.5 text-sm">
      <span className="truncate text-muted-foreground">{original?.key}</span>
      <span className="min-w-0 truncate">
        {original?.value || <span className="text-muted-foreground">—</span>}
      </span>
      {editable ? (
        <div className="flex items-center gap-0.5">
          <Button
            aria-label={`Edit tag ${name}`}
            disabled={pending}
            onClick={() => {
              setEditing(true)
            }}
            size="icon-sm"
            variant="ghost"
          >
            <Pencil />
          </Button>
          <Button
            aria-label={`Remove tag ${name}`}
            disabled={pending}
            onClick={() => {
              void remove()
            }}
            size="icon-sm"
            variant="ghost"
          >
            {pending ? (
              <LoaderCircle className="animate-spin motion-reduce:animate-none" />
            ) : (
              <Trash2 />
            )}
          </Button>
        </div>
      ) : (
        <span />
      )}
      {errorText}
    </div>
  )
}

// A resource's tags, edited a row at a time: the pencil turns a row into two
// inputs and a tick, the bin removes it, and each save is its own request.
export function InlineTags({ tags, onPut, onRemove }: InlineTagsProps) {
  const editable = onPut !== undefined && onRemove !== undefined
  const [adding, setAdding] = useState(false)

  const rows = (tags ?? []).flatMap((tag) =>
    tag.Key ? [{ key: tag.Key, value: tag.Value ?? "" }] : [],
  )

  const keys = rows.map((row) => row.key)

  return (
    <PanelSection title="Tags">
      {rows.length === 0 && !adding && (
        <p className="text-sm text-muted-foreground">No tags.</p>
      )}
      {(rows.length > 0 || adding) && (
        <div className="divide-y">
          {rows.map((row) => (
            <TagRow
              editable={editable}
              // The value is in the key so a refetch resets a row's draft.
              key={`${row.key}=${row.value}`}
              onPut={onPut}
              onRemove={onRemove}
              original={row}
              otherKeys={keys.filter((k) => k !== row.key)}
            />
          ))}
          {adding && (
            <TagRow
              editable
              onDone={() => {
                setAdding(false)
              }}
              onPut={onPut}
              onRemove={onRemove}
              otherKeys={keys}
            />
          )}
        </div>
      )}
      {editable && !adding && (
        <Button
          className="mt-2"
          onClick={() => {
            setAdding(true)
          }}
          size="sm"
          variant="outline"
        >
          <Plus data-icon="inline-start" />
          Add tag
        </Button>
      )}
    </PanelSection>
  )
}
