import type { AttachedPolicy } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import {
  useAttachRolePolicy,
  useDeleteRole,
  useDetachRolePolicy,
} from "@/mutations/iam"
import {
  iamAttachedRolePoliciesQueryOptions,
  iamInstanceProfilesForRoleQueryOptions,
  iamPoliciesQueryOptions,
  iamRoleQueryOptions,
} from "@/queries/iam"

import { PolicyDocumentViewer } from "../../-components/policy-document-viewer"

export const Route = createFileRoute("/_auth/iam/(roles)/list-roles/$roleName")(
  {
    loader: async ({ context, params }) => {
      await Promise.all([
        context.queryClient.ensureQueryData(
          iamRoleQueryOptions(params.roleName),
        ),
        context.queryClient.ensureQueryData(
          iamAttachedRolePoliciesQueryOptions(params.roleName),
        ),
        context.queryClient.ensureQueryData(
          iamInstanceProfilesForRoleQueryOptions(params.roleName),
        ),
        context.queryClient.ensureQueryData(iamPoliciesQueryOptions),
      ])
    },
    head: ({ params }) => ({
      meta: [{ title: `${params.roleName} | IAM | Mulga` }],
    }),
    component: RoleDetail,
  },
)

function RoleDetail() {
  const { roleName } = Route.useParams()
  const navigate = useNavigate()
  const { data: roleData } = useSuspenseQuery(iamRoleQueryOptions(roleName))
  const { data: attachedPoliciesData } = useSuspenseQuery(
    iamAttachedRolePoliciesQueryOptions(roleName),
  )
  const { data: profilesData } = useSuspenseQuery(
    iamInstanceProfilesForRoleQueryOptions(roleName),
  )
  const { data: allPoliciesData } = useSuspenseQuery(iamPoliciesQueryOptions)

  const deleteMutation = useDeleteRole()
  const attachPolicyMutation = useAttachRolePolicy()
  const detachPolicyMutation = useDetachRolePolicy()

  const [showDeleteDialog, setShowDeleteDialog] = useState(false)
  const [showAttachSelect, setShowAttachSelect] = useState(false)
  const [pendingPolicyAction, setPendingPolicyAction] = useState<string | null>(
    null,
  )

  const role = roleData.Role
  const attachedPolicies = attachedPoliciesData.AttachedPolicies ?? []
  const instanceProfiles = profilesData.InstanceProfiles ?? []
  const allPolicies = allPoliciesData.Policies ?? []

  const attachedArns = new Set(attachedPolicies.map((p) => p.PolicyArn))
  const availablePolicies = allPolicies.filter(
    (p) => p.Arn && !attachedArns.has(p.Arn),
  )

  const trustPolicy = role?.AssumeRolePolicyDocument
    ? decodeURIComponent(role.AssumeRolePolicyDocument)
    : null

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(roleName)
      navigate({ to: "/iam/list-roles" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const handleAttachPolicy = async (policyArn: string) => {
    await attachPolicyMutation.mutateAsync({ roleName, policyArn })
    setShowAttachSelect(false)
  }

  const handleDetachPolicy = async (policyArn: string) => {
    setPendingPolicyAction(policyArn)
    try {
      await detachPolicyMutation.mutateAsync({ roleName, policyArn })
    } finally {
      setPendingPolicyAction(null)
    }
  }

  if (!role) {
    return (
      <>
        <BackLink to="/iam/list-roles">Back to roles</BackLink>
        <p className="text-muted-foreground">Role not found.</p>
      </>
    )
  }

  return (
    <>
      <BackLink to="/iam/list-roles">Back to roles</BackLink>

      {deleteMutation.error && (
        <ErrorBanner error={deleteMutation.error} msg="Failed to delete role" />
      )}
      {attachPolicyMutation.error && (
        <ErrorBanner
          error={attachPolicyMutation.error}
          msg="Failed to attach policy"
        />
      )}
      {detachPolicyMutation.error && (
        <ErrorBanner
          error={detachPolicyMutation.error}
          msg="Failed to detach policy"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <Button
              onClick={() => setShowDeleteDialog(true)}
              size="sm"
              variant="destructive"
            >
              <Trash2 className="size-4" />
              Delete
            </Button>
          }
          subtitle="Role Details"
          title={role.RoleName ?? ""}
        />

        <DetailCard>
          <DetailCard.Header>Role Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Role Name" value={role.RoleName} />
            <DetailRow label="Role ID" value={role.RoleId} />
            <DetailRow label="ARN" value={role.Arn} />
            <DetailRow label="Path" value={role.Path} />
            <DetailRow label="Description" value={role.Description ?? "-"} />
            <DetailRow
              label="Created"
              value={formatDateTime(role.CreateDate)}
            />
          </DetailCard.Content>
        </DetailCard>

        {trustPolicy && (
          <DetailCard>
            <DetailCard.Header>Trust Policy</DetailCard.Header>
            <div className="p-4">
              <PolicyDocumentViewer document={trustPolicy} />
            </div>
          </DetailCard>
        )}

        {/* Attached Policies */}
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
            {showAttachSelect && availablePolicies.length > 0 && (
              <div className="col-span-2 space-y-2 rounded-md border p-3">
                <p className="text-sm font-medium">
                  Select a policy to attach:
                </p>
                <div className="space-y-1">
                  {availablePolicies.map((policy) => (
                    <button
                      className="flex w-full items-center justify-between rounded-md p-2 text-left text-sm hover:bg-accent"
                      disabled={attachPolicyMutation.isPending}
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
                      disabled={pendingPolicyAction === policy.PolicyArn}
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

        {/* Instance Profiles */}
        <DetailCard>
          <DetailCard.Header>Instance Profiles</DetailCard.Header>
          <DetailCard.Content>
            {instanceProfiles.length > 0 ? (
              <div className="col-span-2 space-y-3">
                {instanceProfiles.map((profile) => (
                  <Link
                    className="flex items-center justify-between rounded-md border p-3 transition-colors hover:bg-accent"
                    key={profile.InstanceProfileName}
                    params={{
                      instanceProfileName: profile.InstanceProfileName ?? "",
                    }}
                    to="/iam/list-instance-profiles/$instanceProfileName"
                  >
                    <p className="text-sm font-medium">
                      {profile.InstanceProfileName}
                    </p>
                    <span className="text-xs text-muted-foreground">
                      {profile.Arn}
                    </span>
                  </Link>
                ))}
              </div>
            ) : (
              <p className="col-span-2 text-sm text-muted-foreground">
                This role is not in any instance profile.
              </p>
            )}
          </DetailCard.Content>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the role "${roleName}"? This action cannot be undone. You must detach all policies and remove it from all instance profiles first.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Role"
      />
    </>
  )
}
