import type {
  AccessKeyMetadata,
  AttachedPolicy,
  Group,
} from "@aws-sdk/client-iam"
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
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import {
  useAttachUserPolicy,
  useCreateAccessKey,
  useDeleteAccessKey,
  useDeleteUser,
  useDetachUserPolicy,
  useRemoveUserFromGroup,
  useUpdateAccessKey,
} from "@/mutations/iam"
import {
  iamAccessKeysQueryOptions,
  iamAttachedUserPoliciesQueryOptions,
  iamGroupsForUserQueryOptions,
  iamPoliciesQueryOptions,
  iamUserQueryOptions,
} from "@/queries/iam"

import { AccessKeyModal } from "../../-components/access-key-modal"

export const Route = createFileRoute("/_auth/iam/(users)/list-users/$userName")(
  {
    loader: async ({ context, params }) => {
      await Promise.all([
        context.queryClient.ensureQueryData(
          iamUserQueryOptions(params.userName),
        ),
        context.queryClient.ensureQueryData(
          iamAccessKeysQueryOptions(params.userName),
        ),
        context.queryClient.ensureQueryData(
          iamAttachedUserPoliciesQueryOptions(params.userName),
        ),
        context.queryClient.ensureQueryData(iamPoliciesQueryOptions),
        context.queryClient.ensureQueryData(
          iamGroupsForUserQueryOptions(params.userName),
        ),
      ])
    },
    head: ({ params }) => ({
      meta: [{ title: `${params.userName} | IAM | Mulga` }],
    }),
    component: UserDetail,
  },
)

function UserDetail() {
  const { userName } = Route.useParams()
  const navigate = useNavigate()
  const { data: userData } = useSuspenseQuery(iamUserQueryOptions(userName))
  const { data: accessKeysData } = useSuspenseQuery(
    iamAccessKeysQueryOptions(userName),
  )
  const { data: attachedPoliciesData } = useSuspenseQuery(
    iamAttachedUserPoliciesQueryOptions(userName),
  )
  const { data: allPoliciesData } = useSuspenseQuery(iamPoliciesQueryOptions)
  const { data: groupsData } = useSuspenseQuery(
    iamGroupsForUserQueryOptions(userName),
  )

  const deleteMutation = useDeleteUser()
  const createAccessKeyMutation = useCreateAccessKey()
  const deleteAccessKeyMutation = useDeleteAccessKey()
  const updateAccessKeyMutation = useUpdateAccessKey()
  const attachPolicyMutation = useAttachUserPolicy()
  const detachPolicyMutation = useDetachUserPolicy()
  const removeFromGroupMutation = useRemoveUserFromGroup()

  const [showDeleteDialog, setShowDeleteDialog] = useState(false)
  const [newAccessKey, setNewAccessKey] = useState<{
    accessKeyId: string
    secretAccessKey: string
  } | null>(null)
  const [showAttachSelect, setShowAttachSelect] = useState(false)
  const [pendingKeyAction, setPendingKeyAction] = useState<string | null>(null)
  const [pendingPolicyAction, setPendingPolicyAction] = useState<string | null>(
    null,
  )
  const [pendingGroupAction, setPendingGroupAction] = useState<string | null>(
    null,
  )

  const user = userData.User
  const accessKeys = accessKeysData.AccessKeyMetadata ?? []
  const attachedPolicies = attachedPoliciesData.AttachedPolicies ?? []
  const allPolicies = allPoliciesData.Policies ?? []
  const groups = groupsData.Groups ?? []

  const attachedArns = new Set(attachedPolicies.map((p) => p.PolicyArn))
  const availablePolicies = allPolicies.filter(
    (p) => p.Arn && !attachedArns.has(p.Arn),
  )

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(userName)
      navigate({ to: "/iam/list-users" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const handleCreateAccessKey = async () => {
    const response = await createAccessKeyMutation.mutateAsync(userName)
    if (response.AccessKey) {
      setNewAccessKey({
        accessKeyId: response.AccessKey.AccessKeyId ?? "",
        secretAccessKey: response.AccessKey.SecretAccessKey ?? "",
      })
    }
  }

  const handleToggleAccessKey = async (key: AccessKeyMetadata) => {
    if (!key.AccessKeyId) {
      return
    }
    setPendingKeyAction(key.AccessKeyId)
    try {
      await updateAccessKeyMutation.mutateAsync({
        userName,
        accessKeyId: key.AccessKeyId,
        status: key.Status === "Active" ? "Inactive" : "Active",
      })
    } finally {
      setPendingKeyAction(null)
    }
  }

  const handleDeleteAccessKey = async (accessKeyId: string) => {
    setPendingKeyAction(accessKeyId)
    try {
      await deleteAccessKeyMutation.mutateAsync({ userName, accessKeyId })
    } finally {
      setPendingKeyAction(null)
    }
  }

  const handleAttachPolicy = async (policyArn: string) => {
    await attachPolicyMutation.mutateAsync({ userName, policyArn })
    setShowAttachSelect(false)
  }

  const handleDetachPolicy = async (policyArn: string) => {
    setPendingPolicyAction(policyArn)
    try {
      await detachPolicyMutation.mutateAsync({ userName, policyArn })
    } finally {
      setPendingPolicyAction(null)
    }
  }

  const handleRemoveFromGroup = async (groupName: string) => {
    setPendingGroupAction(groupName)
    try {
      await removeFromGroupMutation.mutateAsync({ groupName, userName })
    } finally {
      setPendingGroupAction(null)
    }
  }

  if (!user) {
    return (
      <>
        <BackLink to="/iam/list-users">Back to users</BackLink>
        <p className="text-muted-foreground">User not found.</p>
      </>
    )
  }

  return (
    <>
      <BackLink to="/iam/list-users">Back to users</BackLink>

      {deleteMutation.error && (
        <ErrorBanner error={deleteMutation.error} msg="Failed to delete user" />
      )}
      {createAccessKeyMutation.error && (
        <ErrorBanner
          error={createAccessKeyMutation.error}
          msg="Failed to create access key"
        />
      )}
      {deleteAccessKeyMutation.error && (
        <ErrorBanner
          error={deleteAccessKeyMutation.error}
          msg="Failed to delete access key"
        />
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
      {removeFromGroupMutation.error && (
        <ErrorBanner
          error={removeFromGroupMutation.error}
          msg="Failed to remove from group"
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
          subtitle="User Details"
          title={user.UserName ?? ""}
        />

        <DetailCard>
          <DetailCard.Header>User Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="User Name" value={user.UserName} />
            <DetailRow label="User ID" value={user.UserId} />
            <DetailRow label="ARN" value={user.Arn} />
            <DetailRow label="Path" value={user.Path} />
            <DetailRow
              label="Created"
              value={formatDateTime(user.CreateDate)}
            />
          </DetailCard.Content>
        </DetailCard>

        {/* Access Keys */}
        <DetailCard>
          <DetailCard.Header>
            <div className="flex items-center justify-between">
              <span>Access Keys</span>
              <Button
                disabled={createAccessKeyMutation.isPending}
                onClick={handleCreateAccessKey}
                size="sm"
              >
                {createAccessKeyMutation.isPending
                  ? "Creating..."
                  : "Create Access Key"}
              </Button>
            </div>
          </DetailCard.Header>
          <DetailCard.Content>
            {accessKeys.length > 0 ? (
              <div className="col-span-2 space-y-3">
                {accessKeys.map((key: AccessKeyMetadata) => (
                  <div
                    className="flex items-center justify-between rounded-md border p-3"
                    key={key.AccessKeyId}
                  >
                    <div className="space-y-1">
                      <p className="font-mono text-sm">{key.AccessKeyId}</p>
                      <p className="text-xs text-muted-foreground">
                        Created {formatDateTime(key.CreateDate)}
                      </p>
                    </div>
                    <div className="flex items-center gap-2">
                      <StateBadge state={key.Status} />
                      <Button
                        disabled={pendingKeyAction === key.AccessKeyId}
                        onClick={async () => await handleToggleAccessKey(key)}
                        size="sm"
                        variant="outline"
                      >
                        {key.Status === "Active" ? "Deactivate" : "Activate"}
                      </Button>
                      <Button
                        disabled={pendingKeyAction === key.AccessKeyId}
                        onClick={() => {
                          if (key.AccessKeyId) {
                            void handleDeleteAccessKey(key.AccessKeyId)
                          }
                        }}
                        size="sm"
                        variant="destructive"
                      >
                        Delete
                      </Button>
                    </div>
                  </div>
                ))}
              </div>
            ) : (
              <p className="col-span-2 text-sm text-muted-foreground">
                No access keys.
              </p>
            )}
          </DetailCard.Content>
        </DetailCard>

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

        {/* Groups */}
        <DetailCard>
          <DetailCard.Header>Groups</DetailCard.Header>
          <DetailCard.Content>
            {groups.length > 0 ? (
              <div className="col-span-2 space-y-3">
                {groups.map((group: Group) => (
                  <div
                    className="flex items-center justify-between rounded-md border p-3"
                    key={group.GroupName}
                  >
                    <Link
                      className="text-sm font-medium hover:underline"
                      params={{ groupName: group.GroupName ?? "" }}
                      to="/iam/list-groups/$groupName"
                    >
                      {group.GroupName}
                    </Link>
                    <Button
                      disabled={pendingGroupAction === group.GroupName}
                      onClick={() => {
                        if (group.GroupName) {
                          void handleRemoveFromGroup(group.GroupName)
                        }
                      }}
                      size="sm"
                      variant="outline"
                    >
                      Remove
                    </Button>
                  </div>
                ))}
              </div>
            ) : (
              <p className="col-span-2 text-sm text-muted-foreground">
                Not a member of any group.
              </p>
            )}
          </DetailCard.Content>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the user "${userName}"? This action cannot be undone. You must detach all policies and delete all access keys first.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete User"
      />

      {/* Access Key Secret Modal */}
      {newAccessKey && (
        <AccessKeyModal
          accessKeyId={newAccessKey.accessKeyId}
          onClose={() => setNewAccessKey(null)}
          secretAccessKey={newAccessKey.secretAccessKey}
        />
      )}
    </>
  )
}
