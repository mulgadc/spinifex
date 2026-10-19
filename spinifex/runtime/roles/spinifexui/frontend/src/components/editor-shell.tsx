import type { ReactNode } from "react"

import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"

interface EditorShellProps {
  children: ReactNode
  dirty: boolean
  // Blocks saving without blocking reset, for edits that fail validation.
  invalid?: boolean
  isPending: boolean
  error?: Error | null
  errorMsg: string
  isSuccess?: boolean
  successMsg: string
  dirtyLabel?: string
  notice?: ReactNode
  onReset: () => void
  onSave: () => void
}

// The success notice shows only once the saved values have come back as the
// editor's initial state, so a later unsaved edit hides it again.
export function EditorShell({
  children,
  dirty,
  invalid = false,
  isPending,
  error,
  errorMsg,
  isSuccess,
  successMsg,
  dirtyLabel = "Unsaved changes",
  notice,
  onReset,
  onSave,
}: EditorShellProps) {
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()

        if (!dirty || invalid) {
          return
        }

        onSave()
      }}
    >
      {notice}
      {error && (
        <div role="alert">
          <ErrorBanner error={error} msg={errorMsg} />
        </div>
      )}
      {isSuccess && !dirty && (
        <output className="mb-4 block rounded-md border border-success bg-success/10 p-3 text-sm text-success-foreground">
          {successMsg}
        </output>
      )}
      <div className="rounded-lg border bg-card">
        {children}
        <div className="flex items-center justify-end gap-2 border-t p-4">
          <span className="mr-auto text-xs text-muted-foreground">
            {dirty ? dirtyLabel : "No changes"}
          </span>
          <Button
            disabled={isPending || !dirty}
            onClick={onReset}
            size="sm"
            type="button"
            variant="secondary"
          >
            Reset
          </Button>
          <Button
            disabled={isPending || invalid || !dirty}
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
