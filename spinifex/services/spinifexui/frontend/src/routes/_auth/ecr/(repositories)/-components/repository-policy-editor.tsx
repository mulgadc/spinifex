import { useSuspenseQuery } from "@tanstack/react-query"
import { useEffect, useState } from "react"

import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import {
  useDeleteRepositoryPolicy,
  useSetRepositoryPolicy,
} from "@/mutations/ecr"
import { ecrRepositoryPolicyQueryOptions } from "@/queries/ecr"

interface RepositoryPolicyEditorProps {
  repositoryName: string
}

export function RepositoryPolicyEditor({
  repositoryName,
}: RepositoryPolicyEditorProps) {
  const { data: policyText } = useSuspenseQuery(
    ecrRepositoryPolicyQueryOptions(repositoryName),
  )
  const setPolicy = useSetRepositoryPolicy()
  const deletePolicy = useDeleteRepositoryPolicy()
  const [draft, setDraft] = useState(policyText ?? "")

  // Re-seed the editor when the stored policy changes (save/delete invalidates
  // the query), so the textarea tracks the server document.
  useEffect(() => {
    setDraft(policyText ?? "")
  }, [policyText])

  async function handleSave() {
    try {
      await setPolicy.mutateAsync({ repositoryName, policyText: draft })
    } catch {
      // error shown via setPolicy.error
    }
  }

  async function handleDelete() {
    try {
      await deletePolicy.mutateAsync(repositoryName)
    } catch {
      // error shown via deletePolicy.error
    }
  }

  const error = setPolicy.error ?? deletePolicy.error

  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium">Repository policy</h2>
        <div className="flex gap-2">
          <Button
            disabled={deletePolicy.isPending || !policyText}
            onClick={handleDelete}
            size="sm"
            variant="destructive"
          >
            {deletePolicy.isPending ? "Deleting…" : "Delete"}
          </Button>
          <Button
            disabled={setPolicy.isPending || draft.trim().length === 0}
            onClick={handleSave}
            size="sm"
          >
            {setPolicy.isPending ? "Saving…" : "Save"}
          </Button>
        </div>
      </div>
      <Textarea
        className="font-mono text-xs"
        onChange={(e) => setDraft(e.target.value)}
        placeholder="No policy attached. Paste an IAM policy document to set one."
        rows={10}
        value={draft}
      />
      {error && (
        <p className="mt-2 text-sm text-destructive">{error.message}</p>
      )}
    </div>
  )
}
