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
import { useDeleteRole } from "@/mutations/iam"
import {
  iamAttachedRolePoliciesQueryOptions,
  iamInstanceProfilesForRoleQueryOptions,
  iamPoliciesQueryOptions,
  iamRolePoliciesQueryOptions,
  iamRoleQueryOptions,
} from "@/queries/iam"

import { AttachedPoliciesPanel } from "../../-components/attached-policies-panel"
import { InlinePoliciesPanel } from "../../-components/inline-policies-panel"
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
        context.queryClient.ensureQueryData(
          iamRolePoliciesQueryOptions(params.roleName),
        ),
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
  const { data: profilesData } = useSuspenseQuery(
    iamInstanceProfilesForRoleQueryOptions(roleName),
  )

  const deleteMutation = useDeleteRole()

  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const role = roleData.Role
  const instanceProfiles = profilesData.InstanceProfiles ?? []

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

        <AttachedPoliciesPanel kind="role" name={roleName} />

        <InlinePoliciesPanel kind="role" name={roleName} />

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
