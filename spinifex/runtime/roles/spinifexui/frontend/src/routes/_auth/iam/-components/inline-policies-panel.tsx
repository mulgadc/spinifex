import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Plus } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { JsonEditor } from "@/components/ui/json-editor"
import { isValidJson } from "@/lib/json"
import { useDeleteInlinePolicy, usePutInlinePolicy } from "@/mutations/iam"
import {
  iamGroupPoliciesQueryOptions,
  iamGroupPolicyQueryOptions,
  iamRolePoliciesQueryOptions,
  iamRolePolicyQueryOptions,
  iamUserPoliciesQueryOptions,
  iamUserPolicyQueryOptions,
} from "@/queries/iam"
import { type IamPrincipalKind, putInlinePolicySchema } from "@/types/iam"

const DEFAULT_INLINE_POLICY_DOCUMENT = JSON.stringify(
  {
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Action: "*",
        Resource: "*",
      },
    ],
  },
  null,
  2,
)

type InlinePolicyQueryOptions = ReturnType<typeof iamUserPolicyQueryOptions>

interface KindQueries {
  listQuery: (name: string) => ReturnType<typeof iamUserPoliciesQueryOptions>
  policyQuery: (name: string, policyName: string) => InlinePolicyQueryOptions
}

const kindQueries: Record<IamPrincipalKind, KindQueries> = {
  user: {
    listQuery: iamUserPoliciesQueryOptions,
    policyQuery: iamUserPolicyQueryOptions,
  },
  role: {
    listQuery: iamRolePoliciesQueryOptions,
    policyQuery: iamRolePolicyQueryOptions,
  },
  group: {
    listQuery: iamGroupPoliciesQueryOptions,
    policyQuery: iamGroupPolicyQueryOptions,
  },
}

type PutInlinePolicyMutation = ReturnType<typeof usePutInlinePolicy>

interface InlinePoliciesPanelProps {
  kind: IamPrincipalKind
  name: string
}

export function InlinePoliciesPanel({ kind, name }: InlinePoliciesPanelProps) {
  const put = usePutInlinePolicy(kind)
  const remove = useDeleteInlinePolicy(kind)
  const { listQuery, policyQuery } = kindQueries[kind]

  const { data: listData } = useSuspenseQuery(listQuery(name))
  const policyNames = listData.PolicyNames ?? []

  const [adding, setAdding] = useState(false)
  const [editing, setEditing] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null)

  const handleDelete = async () => {
    if (!confirmDelete) {
      return
    }

    const policyName = confirmDelete

    try {
      await remove.mutateAsync({ name, policyName })

      if (editing === policyName) {
        setEditing(null)
      }
    } catch {
      // Shown by the banner above.
    } finally {
      setConfirmDelete(null)
    }
  }

  return (
    <PanelSection title="Inline policies">
      <div className="flex flex-col gap-3">
        {remove.error && (
          <ErrorBanner
            error={remove.error}
            msg="Failed to delete inline policy"
          />
        )}

        <div>
          <Button
            onClick={() => {
              setAdding((open) => !open)
              setEditing(null)
            }}
            size="sm"
            variant="outline"
          >
            <Plus data-icon="inline-start" />
            Add inline policy
          </Button>
        </div>

        {adding && (
          <AddInlinePolicyForm
            name={name}
            onClose={() => {
              setAdding(false)
            }}
            put={put}
          />
        )}

        {policyNames.length > 0 ? (
          <ul className="divide-y text-sm">
            {policyNames.map((policyName) => (
              <li className="py-2" key={policyName}>
                <div className="flex items-center justify-between gap-3">
                  <p className="min-w-0 truncate font-medium">{policyName}</p>
                  <div className="flex shrink-0 gap-2">
                    <Button
                      onClick={() => {
                        setEditing((current) =>
                          current === policyName ? null : policyName,
                        )
                      }}
                      size="sm"
                      variant="outline"
                    >
                      {editing === policyName ? "Close" : "Edit"}
                    </Button>
                    <Button
                      onClick={() => {
                        setConfirmDelete(policyName)
                      }}
                      size="sm"
                      variant="destructive"
                    >
                      Delete
                    </Button>
                  </div>
                </div>
                {editing === policyName && (
                  <div className="pt-3">
                    <EditInlinePolicyForm
                      name={name}
                      onClose={() => {
                        setEditing(null)
                      }}
                      policyName={policyName}
                      put={put}
                      queryOptions={policyQuery(name, policyName)}
                    />
                  </div>
                )}
              </li>
            ))}
          </ul>
        ) : (
          !adding && (
            <p className="text-sm text-muted-foreground">No inline policies.</p>
          )
        )}
      </div>
      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the inline policy "${confirmDelete ?? ""}"? This action cannot be undone.`}
        isPending={remove.isPending}
        onConfirm={() => {
          void handleDelete()
        }}
        onOpenChange={(open) => {
          if (!open) {
            setConfirmDelete(null)
          }
        }}
        open={confirmDelete !== null}
        title="Delete inline policy"
      />
    </PanelSection>
  )
}

interface AddInlinePolicyFormProps {
  name: string
  put: PutInlinePolicyMutation
  onClose: () => void
}

function AddInlinePolicyForm({ name, put, onClose }: AddInlinePolicyFormProps) {
  const [policyName, setPolicyName] = useState("")
  const [draft, setDraft] = useState(DEFAULT_INLINE_POLICY_DOCUMENT)

  const parsed = putInlinePolicySchema.safeParse({
    policyName,
    policyDocument: draft,
  })

  const nameIssue = parsed.success
    ? undefined
    : parsed.error.issues.find((issue) => issue.path[0] === "policyName")

  const showNameError = policyName.trim().length > 0 && !!nameIssue
  const invalidJson = draft.trim().length > 0 && !isValidJson(draft)

  const handleSave = async () => {
    try {
      await put.mutateAsync({ name, policyName, policyDocument: draft })
      onClose()
    } catch {
      // surfaced via put.error
    }
  }

  return (
    <div className="space-y-3 rounded-md border p-3">
      {put.error && (
        <ErrorBanner error={put.error} msg="Failed to add inline policy" />
      )}
      <Field>
        <FieldTitle>
          <label htmlFor="inline-policy-name">Policy name</label>
        </FieldTitle>
        <Input
          aria-invalid={showNameError}
          id="inline-policy-name"
          onChange={(event) => {
            setPolicyName(event.target.value)
          }}
          placeholder="my-inline-policy"
          value={policyName}
        />
        {showNameError && (
          <FieldError errors={[{ message: nameIssue?.message }]} />
        )}
      </Field>
      <JsonEditor
        size="sm"
        error={invalidJson}
        onChange={setDraft}
        rows={12}
        value={draft}
      />
      {invalidJson && (
        <p className="text-sm text-destructive">Policy must be valid JSON.</p>
      )}
      <div className="flex justify-end gap-2">
        <Button onClick={onClose} size="sm" variant="outline">
          Cancel
        </Button>
        <Button
          disabled={!parsed.success || put.isPending}
          onClick={() => {
            void handleSave()
          }}
          size="sm"
        >
          {put.isPending ? "Saving…" : "Save"}
        </Button>
      </div>
    </div>
  )
}

interface EditInlinePolicyFormProps {
  name: string
  policyName: string
  queryOptions: InlinePolicyQueryOptions
  put: PutInlinePolicyMutation
  onClose: () => void
}

function EditInlinePolicyForm({
  name,
  policyName,
  queryOptions,
  put,
  onClose,
}: EditInlinePolicyFormProps) {
  const { data: savedDocument, isLoading, error } = useQuery(queryOptions)
  const [draft, setDraft] = useState(savedDocument ?? "")

  // Re-seed from the stored document (save invalidates the query) so the
  // textarea tracks the server copy, mirroring the ECR policy editor.
  const [seededDocument, setSeededDocument] = useState(savedDocument)

  if (savedDocument !== seededDocument) {
    setSeededDocument(savedDocument)
    setDraft(savedDocument ?? "")
  }

  const trimmed = draft.trim()
  const invalidJson = trimmed.length > 0 && !isValidJson(draft)

  const handleSave = async () => {
    try {
      await put.mutateAsync({ name, policyName, policyDocument: draft })
      onClose()
    } catch {
      // surfaced via put.error
    }
  }

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading…</p>
  }

  if (error) {
    return <ErrorBanner error={error} msg="Failed to load inline policy" />
  }

  return (
    <div className="space-y-3">
      {put.error && (
        <ErrorBanner error={put.error} msg="Failed to update inline policy" />
      )}
      <JsonEditor
        size="sm"
        error={invalidJson}
        onChange={setDraft}
        rows={12}
        value={draft}
      />
      {invalidJson && (
        <p className="text-sm text-destructive">Policy must be valid JSON.</p>
      )}
      <div className="flex justify-end gap-2">
        <Button onClick={onClose} size="sm" variant="outline">
          Cancel
        </Button>
        <Button
          disabled={put.isPending || trimmed.length === 0 || invalidJson}
          onClick={() => {
            void handleSave()
          }}
          size="sm"
        >
          {put.isPending ? "Saving…" : "Save"}
        </Button>
      </div>
    </div>
  )
}
