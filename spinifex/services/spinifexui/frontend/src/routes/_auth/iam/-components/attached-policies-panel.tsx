import type { AttachedPolicy } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"

import { DetailCard } from "@/components/detail-card"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import {
  useAttachGroupPolicy,
  useAttachRolePolicy,
  useAttachUserPolicy,
  useDetachGroupPolicy,
  useDetachRolePolicy,
  useDetachUserPolicy,
} from "@/mutations/iam"
import {
  iamAttachedGroupPoliciesQueryOptions,
  iamAttachedRolePoliciesQueryOptions,
  iamAttachedUserPoliciesQueryOptions,
  iamPoliciesQueryOptions,
} from "@/queries/iam"

import type { InlinePolicyKind } from "./inline-policies-panel"

// Structural subset of a mutation result so one panel works with the user, role
// and group hooks regardless of their differing command output types.
interface PolicyMutation<TParams> {
  mutateAsync: (params: TParams) => Promise<unknown>
  error: Error | null
}

// Each kind's hooks name the principal differently in their params, so a bound
// mutation pairs a hook with its param shape and exposes one arn-only call.
interface BoundPolicyMutation {
  run: (policyArn: string) => Promise<unknown>
  error: Error | null
}

function bindPolicyMutation<TParams>(
  mutation: PolicyMutation<TParams>,
  toParams: (policyArn: string) => TParams,
): BoundPolicyMutation {
  return {
    run: async (policyArn) => await mutation.mutateAsync(toParams(policyArn)),
    error: mutation.error,
  }
}

interface KindConfig {
  listQuery: (
    name: string,
  ) => ReturnType<typeof iamAttachedUserPoliciesQueryOptions>
  attach: BoundPolicyMutation
  detach: BoundPolicyMutation
}

interface AttachedPoliciesPanelProps {
  kind: InlinePolicyKind
  name: string
}

// Lists the managed policies attached to a user, role or group, and attaches or
// detaches them from the full policy list.
export function AttachedPoliciesPanel({
  kind,
  name,
}: AttachedPoliciesPanelProps) {
  const attachUser = useAttachUserPolicy()
  const attachRole = useAttachRolePolicy()
  const attachGroup = useAttachGroupPolicy()
  const detachUser = useDetachUserPolicy()
  const detachRole = useDetachRolePolicy()
  const detachGroup = useDetachGroupPolicy()

  const config: Record<InlinePolicyKind, KindConfig> = {
    user: {
      listQuery: iamAttachedUserPoliciesQueryOptions,
      attach: bindPolicyMutation(attachUser, (policyArn) => ({
        userName: name,
        policyArn,
      })),
      detach: bindPolicyMutation(detachUser, (policyArn) => ({
        userName: name,
        policyArn,
      })),
    },
    role: {
      listQuery: iamAttachedRolePoliciesQueryOptions,
      attach: bindPolicyMutation(attachRole, (policyArn) => ({
        roleName: name,
        policyArn,
      })),
      detach: bindPolicyMutation(detachRole, (policyArn) => ({
        roleName: name,
        policyArn,
      })),
    },
    group: {
      listQuery: iamAttachedGroupPoliciesQueryOptions,
      attach: bindPolicyMutation(attachGroup, (policyArn) => ({
        groupName: name,
        policyArn,
      })),
      detach: bindPolicyMutation(detachGroup, (policyArn) => ({
        groupName: name,
        policyArn,
      })),
    },
  }
  const { listQuery, attach, detach } = config[kind]

  const { data: attachedData } = useSuspenseQuery(listQuery(name))
  const { data: allPoliciesData } = useSuspenseQuery(iamPoliciesQueryOptions)

  const [showAttachSelect, setShowAttachSelect] = useState(false)
  const [pendingArn, setPendingArn] = useState<string | null>(null)

  const attachedPolicies = attachedData.AttachedPolicies ?? []
  const allPolicies = allPoliciesData.Policies ?? []

  const attachedArns = new Set(attachedPolicies.map((p) => p.PolicyArn))
  const availablePolicies = allPolicies.filter(
    (p) => p.Arn && !attachedArns.has(p.Arn),
  )

  const handleAttachPolicy = async (policyArn: string) => {
    setPendingArn(policyArn)
    try {
      await attach.run(policyArn)
      // Only collapse the select once the attach lands, so a failure leaves the
      // list open for a retry.
      setShowAttachSelect(false)
    } catch {
      // surfaced via attach.error
    } finally {
      setPendingArn(null)
    }
  }

  const handleDetachPolicy = async (policyArn: string) => {
    setPendingArn(policyArn)
    try {
      await detach.run(policyArn)
    } catch {
      // surfaced via detach.error
    } finally {
      setPendingArn(null)
    }
  }

  return (
    <DetailCard>
      <DetailCard.Header>
        <div className="flex items-center justify-between">
          <span>Attached Policies</span>
          <Button
            onClick={() => setShowAttachSelect(!showAttachSelect)}
            size="sm"
          >
            Attach Policy
          </Button>
        </div>
      </DetailCard.Header>
      <DetailCard.Content>
        {attach.error && (
          <div className="col-span-2">
            <ErrorBanner error={attach.error} msg="Failed to attach policy" />
          </div>
        )}
        {detach.error && (
          <div className="col-span-2">
            <ErrorBanner error={detach.error} msg="Failed to detach policy" />
          </div>
        )}

        {showAttachSelect && availablePolicies.length > 0 && (
          <div className="col-span-2 space-y-2 rounded-md border p-3">
            <p className="text-sm font-medium">Select a policy to attach:</p>
            <div className="space-y-1">
              {availablePolicies.map((policy) => (
                <button
                  className="flex w-full items-center justify-between rounded-md p-2 text-left text-sm hover:bg-accent"
                  disabled={pendingArn === policy.Arn}
                  key={policy.Arn}
                  onClick={() => {
                    if (policy.Arn) {
                      void handleAttachPolicy(policy.Arn)
                    }
                  }}
                  type="button"
                >
                  <span>{policy.PolicyName}</span>
                  <span className="text-xs text-muted-foreground">
                    {policy.Arn}
                  </span>
                </button>
              ))}
            </div>
          </div>
        )}
        {showAttachSelect && availablePolicies.length === 0 && (
          <p className="col-span-2 text-sm text-muted-foreground">
            No policies available to attach.
          </p>
        )}
        {attachedPolicies.length > 0 ? (
          <div className="col-span-2 space-y-3">
            {attachedPolicies.map((policy: AttachedPolicy) => (
              <div
                className="flex items-center justify-between rounded-md border p-3"
                key={policy.PolicyArn}
              >
                <div className="space-y-1">
                  <p className="text-sm font-medium">{policy.PolicyName}</p>
                  <p className="text-xs text-muted-foreground">
                    {policy.PolicyArn}
                  </p>
                </div>
                <Button
                  disabled={pendingArn === policy.PolicyArn}
                  onClick={() => {
                    if (policy.PolicyArn) {
                      void handleDetachPolicy(policy.PolicyArn)
                    }
                  }}
                  size="sm"
                  variant="outline"
                >
                  Detach
                </Button>
              </div>
            ))}
          </div>
        ) : (
          <p className="col-span-2 text-sm text-muted-foreground">
            No attached policies.
          </p>
        )}
      </DetailCard.Content>
    </DetailCard>
  )
}
