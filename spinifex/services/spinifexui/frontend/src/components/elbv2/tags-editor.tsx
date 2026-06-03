import type { Tag } from "@aws-sdk/client-elastic-load-balancing-v2"
import { Plus, Trash2 } from "lucide-react"
import { useMemo, useState } from "react"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"

interface TagRow {
  // Stable id so React keys survive key/value edits and row removal.
  id: number
  key: string
  value: string
}

interface TagsEditorProps {
  tags: Tag[]
  onSubmit: (tags: { key: string; value: string }[]) => void
  isPending: boolean
  error?: Error | null
  isSuccess?: boolean
}

function buildInitialRows(tags: Tag[]): TagRow[] {
  return tags.map((t, i) => ({ id: i, key: t.Key ?? "", value: t.Value ?? "" }))
}

function normalize(rows: TagRow[]): { key: string; value: string }[] {
  return rows
    .map((r) => ({ key: r.key.trim(), value: r.value }))
    .filter((r) => r.key.length > 0)
}

export function TagsEditor({
  tags,
  onSubmit,
  isPending,
  error,
  isSuccess,
}: TagsEditorProps) {
  const initialRows = useMemo(() => buildInitialRows(tags), [tags])

  const [rows, setRows] = useState<TagRow[]>(initialRows)
  const [nextId, setNextId] = useState(initialRows.length)

  const setRow = (id: number, patch: Partial<TagRow>) => {
    setRows((prev) => prev.map((r) => (r.id === id ? { ...r, ...patch } : r)))
  }
  const addRow = () => {
    setRows((prev) => [...prev, { id: nextId, key: "", value: "" }])
    setNextId((n) => n + 1)
  }
  const removeRow = (id: number) => {
    setRows((prev) => prev.filter((r) => r.id !== id))
  }
  const handleReset = () => {
    setRows(initialRows)
  }

  // A row with a value but no key is incomplete; duplicate keys are ambiguous.
  const hasBlankKey = rows.some(
    (r) => r.key.trim().length === 0 && r.value.length > 0,
  )
  const normalized = normalize(rows)
  const keys = normalized.map((r) => r.key)
  const hasDuplicate = new Set(keys).size !== keys.length
  const hasError = hasBlankKey || hasDuplicate

  const dirty =
    JSON.stringify(normalized) !== JSON.stringify(normalize(initialRows))

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        if (hasError || !dirty) {
          return
        }
        onSubmit(normalized)
      }}
    >
      {error && (
        <div
          className="mb-4 rounded-md border border-destructive bg-destructive/10 p-3 text-sm text-destructive"
          role="alert"
        >
          {error.message || "Failed to save tags"}
        </div>
      )}
      {isSuccess && !dirty && (
        <div
          className="mb-4 rounded-md border border-emerald-500 bg-emerald-500/10 p-3 text-sm text-emerald-700 dark:text-emerald-400"
          role="status"
        >
          Tags saved.
        </div>
      )}
      <div className="rounded-lg border bg-card">
        <div className="flex flex-col gap-2 p-4">
          {rows.length === 0 && (
            <p className="text-sm text-muted-foreground">No tags.</p>
          )}
          {rows.map((row) => (
            <div className="flex items-center gap-2" key={row.id}>
              <Input
                aria-label="Tag key"
                onChange={(e) => setRow(row.id, { key: e.target.value })}
                placeholder="Key"
                value={row.key}
              />
              <Input
                aria-label="Tag value"
                onChange={(e) => setRow(row.id, { value: e.target.value })}
                placeholder="Value"
                value={row.value}
              />
              <Button
                aria-label="Remove tag"
                onClick={() => removeRow(row.id)}
                size="icon"
                type="button"
                variant="ghost"
              >
                <Trash2 className="size-4" />
              </Button>
            </div>
          ))}
          <div>
            <Button
              onClick={addRow}
              size="sm"
              type="button"
              variant="secondary"
            >
              <Plus className="size-4" /> Add tag
            </Button>
          </div>
          {hasBlankKey && (
            <p className="text-xs text-destructive">
              Every tag with a value needs a key.
            </p>
          )}
          {hasDuplicate && (
            <p className="text-xs text-destructive">Tag keys must be unique.</p>
          )}
        </div>
        <div className="flex items-center justify-end gap-2 border-t p-4">
          <span className="mr-auto text-xs text-muted-foreground">
            {dirty ? "Unsaved changes" : "No changes"}
          </span>
          <Button
            disabled={isPending || !dirty}
            onClick={handleReset}
            size="sm"
            type="button"
            variant="secondary"
          >
            Reset
          </Button>
          <Button
            disabled={isPending || hasError || !dirty}
            size="sm"
            type="submit"
          >
            {isPending ? "Saving…" : "Save changes"}
          </Button>
        </div>
      </div>
    </form>
  )
}
