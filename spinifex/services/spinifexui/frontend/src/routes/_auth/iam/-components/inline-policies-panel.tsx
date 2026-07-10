import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useEffect, useState } from "react"

import { DetailCard } from "@/components/detail-card"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { JsonEditor } from "@/components/ui/json-editor"
import { isValidJson } from "@/lib/json"
import {
  useDeleteGroupPolicy,
  useDeleteRolePolicy,
  useDeleteUserPolicy,
  usePutGroupPolicy,
  usePutRolePolicy,
  usePutUserPolicy,
} from "@/mutations/iam"
import {
  iamGroupPoliciesQueryOptions,
  iamGroupPolicyQueryOptions,
  iamRolePoliciesQueryOptions,
  iamRolePolicyQueryOptions,
  iamUserPoliciesQueryOptions,
  iamUserPolicyQueryOptions,
} from "@/queries/iam"
import {
  type DeleteInlinePolicyParams,
  type PutInlinePolicyParams,
  putInlinePolicySchema,
} from "@/types/iam"

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

export type InlinePolicyKind = "user" | "role" | "group"

type InlinePolicyQueryOptions = ReturnType<typeof iamUserPolicyQueryOptions>

// Structural subset of a mutation result so one form works with the user, role
// and group hooks regardless of their differing command output types.
interface InlineMutation<TParams> {
  mutateAsync: (params: TParams) => Promise<unknown>
  isPending: boolean
  error: Error | null
}

interface KindConfig {
  listQuery: (name: string) => ReturnType<typeof iamUserPoliciesQueryOptions>
  policyQuery: (name: string, policyName: string) => InlinePolicyQueryOptions
  put: InlineMutation<PutInlinePolicyParams>
  remove: InlineMutation<DeleteInlinePolicyParams>
}

interface InlinePoliciesPanelProps {
  kind: InlinePolicyKind
  name: string
}

export function InlinePoliciesPanel({ kind, name }: InlinePoliciesPanelProps) {
  const putUser = usePutUserPolicy()
  const putRole = usePutRolePolicy()
  const putGroup = usePutGroupPolicy()
  const deleteUser = useDeleteUserPolicy()
  const deleteRole = useDeleteRolePolicy()
  const deleteGroup = useDeleteGroupPolicy()

  const config: Record<InlinePolicyKind, KindConfig> = {
    user: {
      listQuery: iamUserPoliciesQueryOptions,
      policyQuery: iamUserPolicyQueryOptions,
      put: putUser,
      remove: deleteUser,
    },
    role: {
      listQuery: iamRolePoliciesQueryOptions,
      policyQuery: iamRolePolicyQueryOptions,
      put: putRole,
      remove: deleteRole,
    },
    group: {
      listQuery: iamGroupPoliciesQueryOptions,
      policyQuery: iamGroupPolicyQueryOptions,
      put: putGroup,
      remove: deleteGroup,
    },
  }
  const { listQuery, policyQuery, put, remove } = config[kind]

  const { data: listData } = useSuspenseQuery(listQuery(name))
  const policyNames = listData.PolicyNames ?? []

  const [adding, setAdding] = useState(false)
  const [editing, setEditing] = useState<string | null>(null)
  const [pendingDelete, setPendingDelete] = useState<string | null>(null)

  const handleDelete = async (policyName: string) => {
    setPendingDelete(policyName)
    try {
      await remove.mutateAsync({ name, policyName })
      if (editing === policyName) {
        setEditing(null)
      }
    } finally {
      setPendingDelete(null)
    }
  }

  return (
    <DetailCard>
      <DetailCard.Header>
        <div className="flex items-center justify-between">
          <span>Inline Policies</span>
          <Button
            onClick={() => {
              setAdding((open) => !open)
              setEditing(null)
            }}
            size="sm"
          >
            Add Inline Policy
          </Button>
        </div>
      </DetailCard.Header>
      <DetailCard.Content>
        {remove.error && (
          <div className="col-span-2">
            <ErrorBanner
              error={remove.error}
              msg="Failed to delete inline policy"
            />
          </div>
        )}

        {adding && (
          <div className="col-span-2">
            <AddInlinePolicyForm
              name={name}
              onClose={() => setAdding(false)}
              put={put}
            />
          </div>
        )}

        {policyNames.length > 0 ? (
          <div className="col-span-2 space-y-3">
            {policyNames.map((policyName) => (
              <div className="rounded-md border" key={policyName}>
                <div className="flex items-center justify-between p-3">
                  <p className="text-sm font-medium">{policyName}</p>
                  <div className="flex gap-2">
                    <Button
                      onClick={() =>
                        setEditing((current) =>
                          current === policyName ? null : policyName,
                        )
                      }
                      size="sm"
                      variant="outline"
                    >
                      {editing === policyName ? "Close" : "Edit"}
                    </Button>
                    <Button
                      disabled={pendingDelete === policyName}
                      onClick={() => void handleDelete(policyName)}
                      size="sm"
                      variant="destructive"
                    >
                      Delete
                    </Button>
                  </div>
                </div>
                {editing === policyName && (
                  <div className="border-t p-3">
                    <EditInlinePolicyForm
                      name={name}
                      onClose={() => setEditing(null)}
                      policyName={policyName}
                      put={put}
                      queryOptions={policyQuery(name, policyName)}
                    />
                  </div>
                )}
              </div>
            ))}
          </div>
        ) : (
          !adding && (
            <p className="col-span-2 text-sm text-muted-foreground">
              No inline policies.
            </p>
          )
        )}
      </DetailCard.Content>
    </DetailCard>
  )
}

interface AddInlinePolicyFormProps {
  name: string
  put: InlineMutation<PutInlinePolicyParams>
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
          onChange={(event) => setPolicyName(event.target.value)}
          placeholder="my-inline-policy"
          value={policyName}
        />
        {showNameError && (
          <FieldError errors={[{ message: nameIssue?.message }]} />
        )}
      </Field>
      <JsonEditor
        className="text-xs"
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
          onClick={() => void handleSave()}
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
  put: InlineMutation<PutInlinePolicyParams>
  onClose: () => void
}

function EditInlinePolicyForm({
  name,
  policyName,
  queryOptions,
  put,
  onClose,
}: EditInlinePolicyFormProps) {
  const { data: savedDocument, isLoading } = useQuery(queryOptions)
  const [draft, setDraft] = useState("")

  // Re-seed from the stored document (save invalidates the query) so the
  // textarea tracks the server copy, mirroring the ECR policy editor.
  useEffect(() => {
    setDraft(savedDocument ?? "")
  }, [savedDocument])

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

  return (
    <div className="space-y-3">
      {put.error && (
        <ErrorBanner error={put.error} msg="Failed to update inline policy" />
      )}
      <JsonEditor
        className="text-xs"
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
          onClick={() => void handleSave()}
          size="sm"
        >
          {put.isPending ? "Saving…" : "Save"}
        </Button>
      </div>
    </div>
  )
}
