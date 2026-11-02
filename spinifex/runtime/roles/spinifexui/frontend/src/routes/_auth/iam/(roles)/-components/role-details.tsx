import type { Role } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { Suspense, useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { IamTags } from "@/components/resource-tags"
import { Button, buttonVariants } from "@/components/ui/button"
import { decodePolicyDocument } from "@/lib/json"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteRole } from "@/mutations/iam"
import {
  iamInstanceProfilesForRoleQueryOptions,
  iamRoleQueryOptions,
} from "@/queries/iam"

import { AttachedPoliciesPanel } from "../../-components/attached-policies-panel"
import { iamDeleteWarning } from "../../-components/delete-prerequisites"
import { IamItem, IamItems } from "../../-components/iam-items"
import { InlinePoliciesPanel } from "../../-components/inline-policies-panel"
import { PolicyDocumentViewer } from "../../-components/policy-document-viewer"

function TrustPolicy({ roleName }: { roleName: string }) {
  const { data } = useSuspenseQuery(iamRoleQueryOptions(roleName))
  const document = data.Role?.AssumeRolePolicyDocument

  return (
    <PanelSection title="Trust policy">
      {document ? (
        <PolicyDocumentViewer document={decodePolicyDocument(document)} />
      ) : (
        <p className="text-sm text-muted-foreground">No trust policy.</p>
      )}
    </PanelSection>
  )
}

function InstanceProfiles({ roleName }: { roleName: string }) {
  const { data } = useSuspenseQuery(
    iamInstanceProfilesForRoleQueryOptions(roleName),
  )

  const profiles = data.InstanceProfiles ?? []

  return (
    <PanelSection title="Instance profiles">
      {profiles.length > 0 ? (
        <IamItems>
          {profiles.map((profile) => (
            <IamItem detail={profile.Arn} key={profile.InstanceProfileName}>
              <Link
                className="text-primary hover:underline"
                params={{
                  instanceProfileName: profile.InstanceProfileName ?? "",
                }}
                to="/iam/list-instance-profiles/$instanceProfileName"
              >
                {profile.InstanceProfileName}
              </Link>
            </IamItem>
          ))}
        </IamItems>
      ) : (
        <p className="text-sm text-muted-foreground">
          This role is not in any instance profile.
        </p>
      )}
    </PanelSection>
  )
}

// ListRoles carries no tags, so they come from GetRole.
function Tags({ roleName }: { roleName: string }) {
  const { data } = useSuspenseQuery(iamRoleQueryOptions(roleName))

  return <IamTags id={roleName} kind="role" tags={data.Role?.Tags} />
}

function Overview({ role }: { role: Role }) {
  const roleName = role.RoleName ?? ""

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Role name">{role.RoleName}</PanelField>
          <PanelField copy={role.RoleId} label="Role ID" mono>
            {role.RoleId}
          </PanelField>
          <PanelField copy={role.Arn} label="ARN" mono>
            {role.Arn}
          </PanelField>
          <PanelField label="Path" mono>
            {role.Path}
          </PanelField>
          <PanelField label="Description">{role.Description}</PanelField>
          <PanelField label="Max session duration">
            {role.MaxSessionDuration === undefined
              ? undefined
              : `${role.MaxSessionDuration / 3600} h`}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(role.CreateDate)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Suspense fallback={<PanelSpinner />}>
        <TrustPolicy roleName={roleName} />
        <AttachedPoliciesPanel kind="role" name={roleName} />
        <InlinePoliciesPanel kind="role" name={roleName} />
        <InstanceProfiles roleName={roleName} />
      </Suspense>
    </>
  )
}

function Settings({
  roleName,
  onDeleted,
}: {
  roleName: string
  onDeleted: () => void
}) {
  const remove = useDeleteRole()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Suspense fallback={<PanelSpinner />}>
        <Tags roleName={roleName} />
      </Suspense>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <p className="text-sm text-muted-foreground">
            Detach its policies and remove it from its instance profiles before
            deleting it.
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
            Delete role
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete the role ${roleName}? ${iamDeleteWarning("role")}`}
        identifier={roleName}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete role"
      />
    </>
  )
}

interface RoleDetailsProps {
  role: Role
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One role's details, as the list's side panel or as its own page.
export function RoleDetails({
  role,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: RoleDetailsProps) {
  const roleName = role.RoleName ?? ""

  return (
    <DetailPanel
      closeLabel={layout === "page" ? "Back to roles" : "Close role details"}
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open role page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ roleName }}
            search={{ tab }}
            to="/iam/list-roles/$roleName"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Role details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={role.RoleId}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview role={role} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} roleName={roleName} />,
        },
      ]}
      title={roleName}
    />
  )
}
