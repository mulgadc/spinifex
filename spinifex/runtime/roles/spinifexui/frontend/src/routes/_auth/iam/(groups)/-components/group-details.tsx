import type { Group } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  Eye,
  Maximize2,
  SlidersHorizontal,
  Trash2,
  UserPlus,
} from "lucide-react"
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
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import {
  useAddUserToGroup,
  useDeleteGroup,
  useRemoveUserFromGroup,
} from "@/mutations/iam"
import { iamGroupQueryOptions, iamUsersQueryOptions } from "@/queries/iam"

import { AttachedPoliciesPanel } from "../../-components/attached-policies-panel"
import { iamDeleteWarning } from "../../-components/delete-prerequisites"
import { IamItem, IamItems } from "../../-components/iam-items"
import { InlinePoliciesPanel } from "../../-components/inline-policies-panel"

// GetGroup is what lists a group's members; ListGroups does not.
function Members({ groupName }: { groupName: string }) {
  const { data: groupData } = useSuspenseQuery(iamGroupQueryOptions(groupName))
  const { data: usersData } = useSuspenseQuery(iamUsersQueryOptions)
  const add = useAddUserToGroup()
  const remove = useRemoveUserFromGroup()
  const [adding, setAdding] = useState(false)
  const [confirm, setConfirm] = useState<string>()

  const members = groupData.Users ?? []
  const memberNames = new Set(members.map((user) => user.UserName))

  const available = (usersData.Users ?? []).filter(
    (user) => user.UserName && !memberNames.has(user.UserName),
  )

  const error = add.error ?? remove.error

  async function onAdd(userName: string) {
    try {
      await add.mutateAsync({ groupName, userName })
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
      await remove.mutateAsync({ groupName, userName: confirm })
    } catch {
      // Shown by the banner above.
    } finally {
      setConfirm(undefined)
    }
  }

  return (
    <PanelSection title="Members">
      <div className="flex flex-col gap-3">
        {error && <ErrorBanner error={error} msg="Membership change failed" />}
        <div>
          <Button
            onClick={() => {
              setAdding(!adding)
            }}
            size="sm"
            variant="outline"
          >
            <UserPlus data-icon="inline-start" />
            Add user
          </Button>
        </div>
        {adding && available.length > 0 && (
          <div className="flex flex-col gap-1 rounded-md border p-2">
            <p className="px-2 text-sm font-medium">Select a user to add:</p>
            {available.map((user) => (
              <button
                className="flex w-full min-w-0 flex-col rounded-md p-2 text-left text-sm hover:bg-accent"
                disabled={add.isPending}
                key={user.UserName}
                onClick={() => {
                  if (user.UserName) {
                    void onAdd(user.UserName)
                  }
                }}
                type="button"
              >
                <span className="truncate">{user.UserName}</span>
                <span className="truncate font-mono text-xs text-muted-foreground">
                  {user.Arn}
                </span>
              </button>
            ))}
          </div>
        )}
        {adding && available.length === 0 && (
          <p className="text-sm text-muted-foreground">
            No users available to add.
          </p>
        )}
        {members.length > 0 ? (
          <IamItems>
            {members.map((user) => (
              <IamItem
                action={
                  <Button
                    disabled={
                      remove.isPending &&
                      remove.variables?.userName === user.UserName
                    }
                    onClick={() => {
                      setConfirm(user.UserName)
                    }}
                    size="sm"
                    variant="outline"
                  >
                    Remove
                  </Button>
                }
                key={user.UserName}
              >
                <Link
                  className="text-primary hover:underline"
                  params={{ userName: user.UserName ?? "" }}
                  to="/iam/list-users/$userName"
                >
                  {user.UserName}
                </Link>
              </IamItem>
            ))}
          </IamItems>
        ) : (
          <p className="text-sm text-muted-foreground">No members.</p>
        )}
      </div>
      <DeleteConfirmationDialog
        confirmLabel="Remove"
        description={`Remove ${confirm ?? "the user"} from ${groupName}? The user loses the permissions the group grants.`}
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
        title="Remove member"
      />
    </PanelSection>
  )
}

function Overview({ group }: { group: Group }) {
  const groupName = group.GroupName ?? ""

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Group name">{group.GroupName}</PanelField>
          <PanelField copy={group.GroupId} label="Group ID" mono>
            {group.GroupId}
          </PanelField>
          <PanelField copy={group.Arn} label="ARN" mono>
            {group.Arn}
          </PanelField>
          <PanelField label="Path" mono>
            {group.Path}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(group.CreateDate)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Suspense fallback={<PanelSpinner />}>
        <Members groupName={groupName} />
        <AttachedPoliciesPanel kind="group" name={groupName} />
        <InlinePoliciesPanel kind="group" name={groupName} />
      </Suspense>
    </>
  )
}

function Settings({
  groupName,
  onDeleted,
}: {
  groupName: string
  onDeleted: () => void
}) {
  const remove = useDeleteGroup()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <p className="text-sm text-muted-foreground">
            Remove its members and detach its policies before deleting it.
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
            Delete group
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete the group ${groupName}? ${iamDeleteWarning("group")}`}
        identifier={groupName}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete group"
      />
    </>
  )
}

interface GroupDetailsProps {
  group: Group
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One group's details, as the list's side panel or as its own page.
export function GroupDetails({
  group,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: GroupDetailsProps) {
  const groupName = group.GroupName ?? ""

  return (
    <DetailPanel
      closeLabel={layout === "page" ? "Back to groups" : "Close group details"}
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open group page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ groupName }}
            search={{ tab }}
            to="/iam/list-groups/$groupName"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Group details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={group.GroupId}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview group={group} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings groupName={groupName} onDeleted={onClose} />,
        },
      ]}
      title={groupName}
    />
  )
}
