import type { AttachedPolicy } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Plus, Unlink } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { useAttachPolicy, useDetachPolicy } from "@/mutations/iam"
import {
  iamAttachedGroupPoliciesQueryOptions,
  iamAttachedRolePoliciesQueryOptions,
  iamAttachedUserPoliciesQueryOptions,
  iamPoliciesQueryOptions,
} from "@/queries/iam"
import type { IamPrincipalKind } from "@/types/iam"

import { IamItem, IamItems } from "./iam-items"

const listQueries: Record<
  IamPrincipalKind,
  (name: string) => ReturnType<typeof iamAttachedUserPoliciesQueryOptions>
> = {
  user: iamAttachedUserPoliciesQueryOptions,
  role: iamAttachedRolePoliciesQueryOptions,
  group: iamAttachedGroupPoliciesQueryOptions,
}

interface AttachedPoliciesPanelProps {
  kind: IamPrincipalKind
  name: string
}

// Lists the managed policies attached to a user, role or group, and attaches or
// detaches them from the full policy list.
export function AttachedPoliciesPanel({
  kind,
  name,
}: AttachedPoliciesPanelProps) {
  const attach = useAttachPolicy(kind)
  const detach = useDetachPolicy(kind)

  const { data: attachedData } = useSuspenseQuery(listQueries[kind](name))
  const { data: allPoliciesData } = useSuspenseQuery(iamPoliciesQueryOptions)

  const [showAttachSelect, setShowAttachSelect] = useState(false)
  const [pendingArn, setPendingArn] = useState<string | null>(null)
  const [confirmDetach, setConfirmDetach] = useState<AttachedPolicy>()

  const attachedPolicies = attachedData.AttachedPolicies ?? []
  const allPolicies = allPoliciesData.Policies ?? []

  const attachedArns = new Set(attachedPolicies.map((p) => p.PolicyArn))

  const availablePolicies = allPolicies.filter(
    (p) => p.Arn && !attachedArns.has(p.Arn),
  )

  const handleAttachPolicy = async (policyArn: string) => {
    setPendingArn(policyArn)

    try {
      await attach.mutateAsync({ name, policyArn })
      // Only collapse the select once the attach lands, so a failure leaves the
      // list open for a retry.
      setShowAttachSelect(false)
    } catch {
      // surfaced via attach.error
    } finally {
      setPendingArn(null)
    }
  }

  const handleDetachPolicy = async () => {
    const policyArn = confirmDetach?.PolicyArn

    if (!policyArn) {
      return
    }

    setPendingArn(policyArn)

    try {
      await detach.mutateAsync({ name, policyArn })
    } catch {
      // surfaced via detach.error
    } finally {
      setPendingArn(null)
      setConfirmDetach(undefined)
    }
  }

  return (
    <PanelSection title="Attached policies">
      <div className="flex flex-col gap-3">
        {attach.error && (
          <ErrorBanner error={attach.error} msg="Failed to attach policy" />
        )}
        {detach.error && (
          <ErrorBanner error={detach.error} msg="Failed to detach policy" />
        )}

        <div>
          <Button
            onClick={() => {
              setShowAttachSelect(!showAttachSelect)
            }}
            size="sm"
            variant="outline"
          >
            <Plus data-icon="inline-start" />
            Attach policy
          </Button>
        </div>

        {showAttachSelect && availablePolicies.length > 0 && (
          <div className="flex flex-col gap-1 rounded-md border p-2">
            <p className="px-2 text-sm font-medium">
              Select a policy to attach:
            </p>
            {availablePolicies.map((policy) => (
              <button
                className="flex w-full min-w-0 flex-col rounded-md p-2 text-left text-sm hover:bg-accent"
                disabled={pendingArn === policy.Arn}
                key={policy.Arn}
                onClick={() => {
                  if (policy.Arn) {
                    void handleAttachPolicy(policy.Arn)
                  }
                }}
                type="button"
              >
                <span className="truncate">{policy.PolicyName}</span>
                <span className="truncate font-mono text-xs text-muted-foreground">
                  {policy.Arn}
                </span>
              </button>
            ))}
          </div>
        )}
        {showAttachSelect && availablePolicies.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No policies available to attach.
          </p>
        )}

        {attachedPolicies.length > 0 ? (
          <IamItems>
            {attachedPolicies.map((policy) => (
              <IamItem
                action={
                  <Button
                    disabled={pendingArn === policy.PolicyArn}
                    onClick={() => {
                      setConfirmDetach(policy)
                    }}
                    size="sm"
                    variant="outline"
                  >
                    <Unlink data-icon="inline-start" />
                    Detach
                  </Button>
                }
                detail={policy.PolicyArn}
                key={policy.PolicyArn}
              >
                <Link
                  className="text-primary hover:underline"
                  params={{
                    policyArn: encodeURIComponent(policy.PolicyArn ?? ""),
                  }}
                  to="/iam/list-policies/$policyArn"
                >
                  {policy.PolicyName}
                </Link>
              </IamItem>
            ))}
          </IamItems>
        ) : (
          <p className="text-sm text-muted-foreground">No attached policies.</p>
        )}
      </div>
      <DeleteConfirmationDialog
        confirmLabel="Detach"
        description={`Detach ${confirmDetach?.PolicyName ?? "the policy"} from ${name}? It loses the permissions the policy grants.`}
        isPending={detach.isPending}
        onConfirm={() => {
          void handleDetachPolicy()
        }}
        onOpenChange={(open) => {
          if (!open) {
            setConfirmDetach(undefined)
          }
        }}
        open={confirmDetach !== undefined}
        pendingLabel="Detaching…"
        title="Detach policy"
      />
    </PanelSection>
  )
}
