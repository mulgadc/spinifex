import type { InstanceProfile } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, Plus, SlidersHorizontal, Trash2 } from "lucide-react"
import { Suspense, useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { IamTags } from "@/components/resource-tags"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
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

import { iamDeleteWarning } from "../../-components/delete-prerequisites"
import { IamItem, IamItems } from "../../-components/iam-items"

// The roles come from GetInstanceProfile, so an add or remove shows at once.
function Roles({ instanceProfileName }: { instanceProfileName: string }) {
  const { data: profileData } = useSuspenseQuery(
    iamInstanceProfileQueryOptions(instanceProfileName),
  )

  const { data: rolesData } = useSuspenseQuery(iamRolesQueryOptions)
  const add = useAddRoleToInstanceProfile()
  const remove = useRemoveRoleFromInstanceProfile()
  const [adding, setAdding] = useState(false)
  const [confirm, setConfirm] = useState<string>()

  const roles = profileData.InstanceProfile?.Roles ?? []
  const attached = new Set(roles.map((role) => role.RoleName))

  const available = (rolesData.Roles ?? []).filter(
    (role) => role.RoleName && !attached.has(role.RoleName),
  )

  const error = add.error ?? remove.error

  async function onAdd(roleName: string) {
    try {
      await add.mutateAsync({ instanceProfileName, roleName })
      setAdding(false)
    } catch {
      // Shown by the banner above.
    }
  }

  async function onRemove() {
    if (!confirm) {
      return
    }

    try {
      await remove.mutateAsync({ instanceProfileName, roleName: confirm })
    } catch {
      // Shown by the banner above.
    } finally {
      setConfirm(undefined)
    }
  }

  return (
    <PanelSection title="Roles">
      <div className="flex flex-col gap-3">
        {error && <ErrorBanner error={error} msg="Role change failed" />}
        <div>
          <Button
            onClick={() => {
              setAdding(!adding)
            }}
            size="sm"
            variant="outline"
          >
            <Plus data-icon="inline-start" />
            Add role
          </Button>
        </div>
        {adding && available.length > 0 && (
          <div className="flex flex-col gap-1 rounded-md border p-2">
            <p className="px-2 text-sm font-medium">Select a role to add:</p>
            {available.map((role) => (
              <button
                className="flex w-full min-w-0 flex-col rounded-md p-2 text-left text-sm hover:bg-accent"
                disabled={add.isPending}
                key={role.RoleName}
                onClick={() => {
                  if (role.RoleName) {
                    void onAdd(role.RoleName)
                  }
                }}
                type="button"
              >
                <span className="truncate">{role.RoleName}</span>
                <span className="truncate font-mono text-xs text-muted-foreground">
                  {role.Arn}
                </span>
              </button>
            ))}
          </div>
        )}
        {adding && available.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No roles available to add.
          </p>
        )}
        {roles.length > 0 ? (
          <IamItems>
            {roles.map((role) => (
              <IamItem
                action={
                  <Button
                    disabled={
                      remove.isPending &&
                      remove.variables?.roleName === role.RoleName
                    }
                    onClick={() => {
                      setConfirm(role.RoleName)
                    }}
                    size="sm"
                    variant="outline"
                  >
                    Remove
                  </Button>
                }
                detail={role.Arn}
                key={role.RoleName}
              >
                <Link
                  className="text-primary hover:underline"
                  params={{ roleName: role.RoleName ?? "" }}
                  to="/iam/list-roles/$roleName"
                >
                  {role.RoleName}
                </Link>
              </IamItem>
            ))}
          </IamItems>
        ) : (
          <p className="text-sm text-muted-foreground">
            No roles in this instance profile.
          </p>
        )}
      </div>
      <DeleteConfirmationDialog
        confirmLabel="Remove"
        description={`Remove ${confirm ?? "the role"} from ${instanceProfileName}? Instances launched with this profile lose the role's credentials.`}
        isPending={remove.isPending}
        onConfirm={() => {
          void onRemove()
        }}
        onOpenChange={(open) => {
          if (!open) {
            setConfirm(undefined)
          }
        }}
        open={confirm !== undefined}
        pendingLabel="Removing…"
        title="Remove role"
      />
    </PanelSection>
  )
}

function Tags({ instanceProfileName }: { instanceProfileName: string }) {
  const { data } = useSuspenseQuery(
    iamInstanceProfileQueryOptions(instanceProfileName),
  )

  return (
    <IamTags
      id={instanceProfileName}
      kind="instance-profile"
      tags={data.InstanceProfile?.Tags}
    />
  )
}

function Overview({ profile }: { profile: InstanceProfile }) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Name">{profile.InstanceProfileName}</PanelField>
          <PanelField copy={profile.InstanceProfileId} label="ID" mono>
            {profile.InstanceProfileId}
          </PanelField>
          <PanelField copy={profile.Arn} label="ARN" mono>
            {profile.Arn}
          </PanelField>
          <PanelField label="Path" mono>
            {profile.Path}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(profile.CreateDate)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Suspense fallback={<PanelSpinner />}>
        <Roles instanceProfileName={profile.InstanceProfileName ?? ""} />
      </Suspense>
    </>
  )
}

function Settings({
  instanceProfileName,
  onDeleted,
}: {
  instanceProfileName: string
  onDeleted: () => void
}) {
  const remove = useDeleteInstanceProfile()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Suspense fallback={<PanelSpinner />}>
        <Tags instanceProfileName={instanceProfileName} />
      </Suspense>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <p className="text-sm text-muted-foreground">
            Remove its role before deleting it.
          </p>
          <Button
            disabled={remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete instance profile
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete the instance profile ${instanceProfileName}? ${iamDeleteWarning("instance profile")}`}
        identifier={instanceProfileName}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete instance profile"
      />
    </>
  )
}

interface InstanceProfileDetailsProps {
  profile: InstanceProfile
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One instance profile's details, as the list's side panel or as its own page.
export function InstanceProfileDetails({
  profile,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: InstanceProfileDetailsProps) {
  const instanceProfileName = profile.InstanceProfileName ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to instance profiles"
          : "Close instance profile details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open instance profile page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ instanceProfileName }}
            search={{ tab }}
            to="/iam/list-instance-profiles/$instanceProfileName"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Instance profile details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={profile.InstanceProfileId}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview profile={profile} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: (
            <Settings
              instanceProfileName={instanceProfileName}
              onDeleted={onClose}
            />
          ),
        },
      ]}
      title={instanceProfileName}
    />
  )
}
