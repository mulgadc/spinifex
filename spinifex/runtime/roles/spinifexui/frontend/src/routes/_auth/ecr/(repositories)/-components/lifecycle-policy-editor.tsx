import type { LifecyclePolicyPreviewResult } from "@aws-sdk/client-ecr"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { Button } from "@/components/ui/button"
import { JsonEditor } from "@/components/ui/json-editor"
import { formatJson, isValidJson } from "@/lib/json"
import { formatDateTime } from "@/lib/utils"
import {
  useDeleteLifecyclePolicy,
  usePreviewLifecyclePolicy,
  usePutLifecyclePolicy,
} from "@/mutations/ecr"
import { ecrLifecyclePolicyQueryOptions } from "@/queries/ecr"

const PREVIEW_COLUMNS: DataColumn<LifecyclePolicyPreviewResult>[] = [
  {
    id: "tags",
    header: "Tags",
    sortValue: (result) => result.imageTags?.join(", "),
    cell: (result) =>
      result.imageTags && result.imageTags.length > 0
        ? result.imageTags.join(", ")
        : "<untagged>",
  },
  {
    id: "digest",
    header: "Digest",
    sortValue: (result) => result.imageDigest,
    hideBelow: "sm",
    cell: (result) => (
      <span className="font-mono text-xs" title={result.imageDigest}>
        {result.imageDigest?.slice(0, 19)}…
      </span>
    ),
  },
  {
    id: "pushed",
    header: "Pushed",
    sortValue: (result) => result.imagePushedAt?.getTime(),
    hideBelow: "md",
    cell: (result) => (
      <span className="tabular-nums">
        {formatDateTime(result.imagePushedAt)}
      </span>
    ),
  },
  {
    id: "rule",
    header: "Rule",
    sortValue: (result) => result.appliedRulePriority,
    align: "end",
    cell: (result) => (
      <span className="tabular-nums">{result.appliedRulePriority}</span>
    ),
  },
]

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

// The server stores the policy compacted; indent it for editing, keeping the
// raw text when it does not parse.
function toDraft(policyText: string | null | undefined): string {
  if (!policyText) {
    return ""
  }

  return formatJson(policyText) ?? policyText
}

export function LifecyclePolicyEditor({
  repositoryName,
}: LifecyclePolicyEditorProps) {
  const { data: policyText } = useSuspenseQuery(
    ecrLifecyclePolicyQueryOptions(repositoryName),
  )

  const putPolicy = usePutLifecyclePolicy()
  const deletePolicy = useDeleteLifecyclePolicy()
  const preview = usePreviewLifecyclePolicy()
  const [draft, setDraft] = useState(() => toDraft(policyText))
  const [sort, setSort] = useState<SortState>()

  // Re-seed the editor when the stored policy changes (save/delete invalidates
  // the query), so the textarea tracks the server document.
  const [seededPolicy, setSeededPolicy] = useState(policyText)

  if (policyText !== seededPolicy) {
    setSeededPolicy(policyText)
    setDraft(toDraft(policyText))
  }

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

  const trimmed = draft.trim()
  const invalidJson = trimmed.length > 0 && !isValidJson(draft)
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
              disabled={
                putPolicy.isPending || trimmed.length === 0 || invalidJson
              }
              onClick={handleSave}
              size="sm"
            >
              {putPolicy.isPending ? "Saving…" : "Save"}
            </Button>
          </div>
        </div>
        <JsonEditor
          size="sm"
          error={invalidJson}
          onChange={setDraft}
          placeholder={SAMPLE_POLICY}
          rows={14}
          value={draft}
        />
        {invalidJson && (
          <p className="mt-2 text-sm text-destructive">
            Lifecycle policy must be valid JSON.
          </p>
        )}
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
          <DataTable
            caption="Images the policy would expire"
            columns={PREVIEW_COLUMNS}
            empty="No images match this policy."
            getRowId={(result) => result.imageDigest ?? ""}
            onSortChange={setSort}
            rows={results}
            sort={sort}
          />
        </div>
      )}
    </div>
  )
}
