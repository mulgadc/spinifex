import type { LifecyclePolicyPreviewResult } from "@aws-sdk/client-ecr"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useEffect, useState } from "react"

import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { formatDateTime } from "@/lib/utils"
import {
  useDeleteLifecyclePolicy,
  usePreviewLifecyclePolicy,
  usePutLifecyclePolicy,
} from "@/mutations/ecr"
import { ecrLifecyclePolicyQueryOptions } from "@/queries/ecr"

interface LifecyclePolicyEditorProps {
  repositoryName: string
}

const SAMPLE_POLICY = `{
  "rules": [
    {
      "rulePriority": 1,
      "description": "Expire untagged images older than 14 days",
      "selection": {
        "tagStatus": "untagged",
        "countType": "sinceImagePushed",
        "countUnit": "days",
        "countNumber": 14
      },
      "action": { "type": "expire" }
    }
  ]
}`

export function LifecyclePolicyEditor({
  repositoryName,
}: LifecyclePolicyEditorProps) {
  const { data: policyText } = useSuspenseQuery(
    ecrLifecyclePolicyQueryOptions(repositoryName),
  )
  const putPolicy = usePutLifecyclePolicy()
  const deletePolicy = useDeleteLifecyclePolicy()
  const preview = usePreviewLifecyclePolicy()
  const [draft, setDraft] = useState(policyText ?? "")

  // Re-seed the editor when the stored policy changes (save/delete invalidates
  // the query), so the textarea tracks the server document.
  useEffect(() => {
    setDraft(policyText ?? "")
  }, [policyText])

  async function handleSave() {
    try {
      await putPolicy.mutateAsync({
        repositoryName,
        lifecyclePolicyText: draft,
      })
    } catch {
      // error shown via putPolicy.error
    }
  }

  async function handleDelete() {
    try {
      await deletePolicy.mutateAsync(repositoryName)
    } catch {
      // error shown via deletePolicy.error
    }
  }

  async function handlePreview() {
    try {
      await preview.mutateAsync(repositoryName)
    } catch {
      // error shown via preview.error
    }
  }

  const error = putPolicy.error ?? deletePolicy.error ?? preview.error
  const results = preview.data?.previewResults ?? []

  return (
    <div className="space-y-4">
      <div className="rounded-lg border bg-card p-4">
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-sm font-medium">Lifecycle policy</h2>
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
              disabled={preview.isPending || !policyText}
              onClick={handlePreview}
              size="sm"
              variant="outline"
            >
              {preview.isPending ? "Previewing…" : "Preview"}
            </Button>
            <Button
              disabled={putPolicy.isPending || draft.trim().length === 0}
              onClick={handleSave}
              size="sm"
            >
              {putPolicy.isPending ? "Saving…" : "Save"}
            </Button>
          </div>
        </div>
        <Textarea
          className="font-mono text-xs"
          onChange={(e) => setDraft(e.target.value)}
          placeholder={SAMPLE_POLICY}
          rows={14}
          value={draft}
        />
        {error && (
          <p className="mt-2 text-sm text-destructive">{error.message}</p>
        )}
      </div>

      {preview.isSuccess && (
        <div className="rounded-lg border bg-card p-4">
          <h3 className="mb-2 text-sm font-medium">
            Preview: {results.length} image
            {results.length === 1 ? "" : "s"} would expire
          </h3>
          {results.length > 0 ? (
            <div className="overflow-x-auto rounded-lg border">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-muted-foreground">
                    <th className="px-4 py-2 font-medium">Tags</th>
                    <th className="px-4 py-2 font-medium">Digest</th>
                    <th className="px-4 py-2 font-medium">Pushed</th>
                    <th className="px-4 py-2 font-medium">Rule</th>
                  </tr>
                </thead>
                <tbody>
                  {results.map((r: LifecyclePolicyPreviewResult) => (
                    <tr className="border-b last:border-0" key={r.imageDigest}>
                      <td className="px-4 py-2">
                        {r.imageTags && r.imageTags.length > 0
                          ? r.imageTags.join(", ")
                          : "<untagged>"}
                      </td>
                      <td className="px-4 py-2 font-mono text-xs">
                        {r.imageDigest?.slice(0, 19)}…
                      </td>
                      <td className="px-4 py-2 text-xs">
                        {formatDateTime(r.imagePushedAt)}
                      </td>
                      <td className="px-4 py-2">{r.appliedRulePriority}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p className="text-muted-foreground">
              No images match this policy.
            </p>
          )}
        </div>
      )}
    </div>
  )
}
