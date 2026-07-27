import type { User } from "@aws-sdk/client-iam"
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
  useAddUserToGroup,
  useDeleteGroup,
  useRemoveUserFromGroup,
} from "@/mutations/iam"
import {
  iamAttachedGroupPoliciesQueryOptions,
  iamGroupPoliciesQueryOptions,
  iamGroupQueryOptions,
  iamPoliciesQueryOptions,
  iamUsersQueryOptions,
} from "@/queries/iam"

import { AttachedPoliciesPanel } from "../../-components/attached-policies-panel"
import { InlinePoliciesPanel } from "../../-components/inline-policies-panel"

export const Route = createFileRoute(
  "/_auth/iam/(groups)/list-groups/$groupName",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(
        iamGroupQueryOptions(params.groupName),
      ),
      context.queryClient.ensureQueryData(
        iamAttachedGroupPoliciesQueryOptions(params.groupName),
      ),
      context.queryClient.ensureQueryData(iamPoliciesQueryOptions),
      context.queryClient.ensureQueryData(iamUsersQueryOptions),
      context.queryClient.ensureQueryData(
        iamGroupPoliciesQueryOptions(params.groupName),
      ),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.groupName} | IAM | Mulga` }],
  }),
  component: GroupDetail,
})

function GroupDetail() {
  const { groupName } = Route.useParams()
  const navigate = useNavigate()
  const { data: groupData } = useSuspenseQuery(iamGroupQueryOptions(groupName))
  const { data: allUsersData } = useSuspenseQuery(iamUsersQueryOptions)

  const deleteMutation = useDeleteGroup()
  const addUserMutation = useAddUserToGroup()
  const removeUserMutation = useRemoveUserFromGroup()

  const [showDeleteDialog, setShowDeleteDialog] = useState(false)
  const [showAddUserSelect, setShowAddUserSelect] = useState(false)
  const [pendingMemberAction, setPendingMemberAction] = useState<string | null>(
    null,
  )

  const group = groupData.Group
  const members = groupData.Users ?? []
  const allUsers = allUsersData.Users ?? []

  const memberNames = new Set(members.map((u) => u.UserName))
  const availableUsers = allUsers.filter(
    (u) => u.UserName && !memberNames.has(u.UserName),
  )

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(groupName)
      navigate({ to: "/iam/list-groups" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const handleAddUser = async (userName: string) => {
    await addUserMutation.mutateAsync({ groupName, userName })
    setShowAddUserSelect(false)
  }

  const handleRemoveUser = async (userName: string) => {
    setPendingMemberAction(userName)
    try {
      await removeUserMutation.mutateAsync({ groupName, userName })
    } finally {
      setPendingMemberAction(null)
    }
  }

  if (!group) {
    return (
      <>
        <BackLink to="/iam/list-groups">Back to groups</BackLink>
        <p className="text-muted-foreground">Group not found.</p>
      </>
    )
  }

  return (
    <>
      <BackLink to="/iam/list-groups">Back to groups</BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete group"
        />
      )}
      {addUserMutation.error && (
        <ErrorBanner error={addUserMutation.error} msg="Failed to add user" />
      )}
      {removeUserMutation.error && (
        <ErrorBanner
          error={removeUserMutation.error}
          msg="Failed to remove user"
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
          subtitle="Group Details"
          title={group.GroupName ?? ""}
        />

        <DetailCard>
          <DetailCard.Header>Group Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Group Name" value={group.GroupName} />
            <DetailRow label="Group ID" value={group.GroupId} />
            <DetailRow label="ARN" value={group.Arn} />
            <DetailRow label="Path" value={group.Path} />
            <DetailRow
              label="Created"
              value={formatDateTime(group.CreateDate)}
            />
          </DetailCard.Content>
        </DetailCard>

        {/* Members */}
        <DetailCard>
          <DetailCard.Header>
            <div className="flex items-center justify-between">
              <span>Members</span>
              <Button
                onClick={() => setShowAddUserSelect(!showAddUserSelect)}
                size="sm"
              >
                Add User
              </Button>
            </div>
          </DetailCard.Header>
          <DetailCard.Content>
            {showAddUserSelect && availableUsers.length > 0 && (
              <div className="col-span-2 space-y-2 rounded-md border p-3">
                <p className="text-sm font-medium">Select a user to add:</p>
                <div className="space-y-1">
                  {availableUsers.map((user) => (
                    <button
                      className="flex w-full items-center justify-between rounded-md p-2 text-left text-sm hover:bg-accent"
                      disabled={addUserMutation.isPending}
                      key={user.UserName}
                      onClick={() => {
                        if (user.UserName) {
                          void handleAddUser(user.UserName)
                        }
                      }}
                      type="button"
                    >
                      <span>{user.UserName}</span>
                      <span className="text-xs text-muted-foreground">
                        {user.Arn}
                      </span>
                    </button>
                  ))}
                </div>
              </div>
            )}
            {showAddUserSelect && availableUsers.length === 0 && (
              <p className="col-span-2 text-sm text-muted-foreground">
                No users available to add.
              </p>
            )}
            {members.length > 0 ? (
              <div className="col-span-2 space-y-3">
                {members.map((user: User) => (
                  <div
                    className="flex items-center justify-between rounded-md border p-3"
                    key={user.UserName}
                  >
                    <Link
                      className="text-sm font-medium hover:underline"
                      params={{ userName: user.UserName ?? "" }}
                      to="/iam/list-users/$userName"
                    >
                      {user.UserName}
                    </Link>
                    <Button
                      disabled={pendingMemberAction === user.UserName}
                      onClick={() => {
                        if (user.UserName) {
                          void handleRemoveUser(user.UserName)
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
                No members.
              </p>
            )}
          </DetailCard.Content>
        </DetailCard>

        <AttachedPoliciesPanel kind="group" name={groupName} />

        <InlinePoliciesPanel kind="group" name={groupName} />
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the group "${groupName}"? This action cannot be undone. You must remove all members and detach all policies first.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Group"
      />
    </>
  )
}
