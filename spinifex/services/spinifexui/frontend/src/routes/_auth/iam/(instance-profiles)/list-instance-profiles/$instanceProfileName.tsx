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
  useAddRoleToInstanceProfile,
  useDeleteInstanceProfile,
  useRemoveRoleFromInstanceProfile,
} from "@/mutations/iam"
import {
  iamInstanceProfileQueryOptions,
  iamRolesQueryOptions,
} from "@/queries/iam"

export const Route = createFileRoute(
  "/_auth/iam/(instance-profiles)/list-instance-profiles/$instanceProfileName",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(
        iamInstanceProfileQueryOptions(params.instanceProfileName),
      ),
      context.queryClient.ensureQueryData(iamRolesQueryOptions),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.instanceProfileName} | IAM | Mulga` }],
  }),
  component: InstanceProfileDetail,
})

function InstanceProfileDetail() {
  const { instanceProfileName } = Route.useParams()
  const navigate = useNavigate()
  const { data: profileData } = useSuspenseQuery(
    iamInstanceProfileQueryOptions(instanceProfileName),
  )
  const { data: rolesData } = useSuspenseQuery(iamRolesQueryOptions)

  const deleteMutation = useDeleteInstanceProfile()
  const addRoleMutation = useAddRoleToInstanceProfile()
  const removeRoleMutation = useRemoveRoleFromInstanceProfile()

  const [showDeleteDialog, setShowDeleteDialog] = useState(false)
  const [showAddSelect, setShowAddSelect] = useState(false)
  const [pendingRoleAction, setPendingRoleAction] = useState<string | null>(
    null,
  )

  const profile = profileData.InstanceProfile
  const profileRoles = profile?.Roles ?? []
  const allRoles = rolesData.Roles ?? []

  const attachedRoleNames = new Set(profileRoles.map((r) => r.RoleName))
  const availableRoles = allRoles.filter(
    (r) => r.RoleName && !attachedRoleNames.has(r.RoleName),
  )

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(instanceProfileName)
      navigate({ to: "/iam/list-instance-profiles" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const handleAddRole = async (roleName: string) => {
    await addRoleMutation.mutateAsync({ instanceProfileName, roleName })
    setShowAddSelect(false)
  }

  const handleRemoveRole = async (roleName: string) => {
    setPendingRoleAction(roleName)
    try {
      await removeRoleMutation.mutateAsync({ instanceProfileName, roleName })
    } finally {
      setPendingRoleAction(null)
    }
  }

  if (!profile) {
    return (
      <>
        <BackLink to="/iam/list-instance-profiles">
          Back to instance profiles
        </BackLink>
        <p className="text-muted-foreground">Instance profile not found.</p>
      </>
    )
  }

  return (
    <>
      <BackLink to="/iam/list-instance-profiles">
        Back to instance profiles
      </BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete instance profile"
        />
      )}
      {addRoleMutation.error && (
        <ErrorBanner error={addRoleMutation.error} msg="Failed to add role" />
      )}
      {removeRoleMutation.error && (
        <ErrorBanner
          error={removeRoleMutation.error}
          msg="Failed to remove role"
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
          subtitle="Instance Profile Details"
          title={profile.InstanceProfileName ?? ""}
        />

        <DetailCard>
          <DetailCard.Header>Instance Profile Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Name" value={profile.InstanceProfileName} />
            <DetailRow label="ID" value={profile.InstanceProfileId} />
            <DetailRow label="ARN" value={profile.Arn} />
            <DetailRow label="Path" value={profile.Path} />
            <DetailRow
              label="Created"
              value={formatDateTime(profile.CreateDate)}
            />
          </DetailCard.Content>
        </DetailCard>

        {/* Roles */}
        <DetailCard>
          <DetailCard.Header>
            <div className="flex items-center justify-between">
              <span>Roles</span>
              <Button
                onClick={() => setShowAddSelect(!showAddSelect)}
                size="sm"
              >
                Add Role
              </Button>
            </div>
          </DetailCard.Header>
          <DetailCard.Content>
            {showAddSelect && availableRoles.length > 0 && (
              <div className="col-span-2 space-y-2 rounded-md border p-3">
                <p className="text-sm font-medium">Select a role to add:</p>
                <div className="space-y-1">
                  {availableRoles.map((role) => (
                    <button
                      className="flex w-full items-center justify-between rounded-md p-2 text-left text-sm hover:bg-accent"
                      disabled={addRoleMutation.isPending}
                      key={role.RoleName}
                      onClick={() => {
                        if (role.RoleName) {
                          void handleAddRole(role.RoleName)
                        }
                      }}
                      type="button"
                    >
                      <span>{role.RoleName}</span>
                      <span className="text-xs text-muted-foreground">
                        {role.Arn}
                      </span>
                    </button>
                  ))}
                </div>
              </div>
            )}
            {showAddSelect && availableRoles.length === 0 && (
              <p className="col-span-2 text-sm text-muted-foreground">
                No roles available to add.
              </p>
            )}
            {profileRoles.length > 0 ? (
              <div className="col-span-2 space-y-3">
                {profileRoles.map((role) => (
                  <div
                    className="flex items-center justify-between rounded-md border p-3"
                    key={role.RoleName}
                  >
                    <Link
                      className="text-sm font-medium text-primary hover:underline"
                      params={{ roleName: role.RoleName ?? "" }}
                      to="/iam/list-roles/$roleName"
                    >
                      {role.RoleName}
                    </Link>
                    <Button
                      disabled={pendingRoleAction === role.RoleName}
                      onClick={() => {
                        if (role.RoleName) {
                          void handleRemoveRole(role.RoleName)
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
                No roles in this instance profile.
              </p>
            )}
          </DetailCard.Content>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the instance profile "${instanceProfileName}"? This action cannot be undone. You must remove all roles first.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Instance Profile"
      />
    </>
  )
}
