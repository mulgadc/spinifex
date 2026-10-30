import type { AccessKeyMetadata, User } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  Eye,
  KeyRound,
  Maximize2,
  SlidersHorizontal,
  Trash2,
} from "lucide-react"
import { Suspense, useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
  PanelTags,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import {
  useCreateAccessKey,
  useDeleteAccessKey,
  useDeleteUser,
  useRemoveUserFromGroup,
  useUpdateAccessKey,
} from "@/mutations/iam"
import {
  iamAccessKeysQueryOptions,
  iamGroupsForUserQueryOptions,
  iamUserQueryOptions,
} from "@/queries/iam"

import { AccessKeyModal } from "../../-components/access-key-modal"
import { AttachedPoliciesPanel } from "../../-components/attached-policies-panel"
import { iamDeleteWarning } from "../../-components/delete-prerequisites"
import { IamItem, IamItems } from "../../-components/iam-items"
import { InlinePoliciesPanel } from "../../-components/inline-policies-panel"

function Groups({ userName }: { userName: string }) {
  const { data } = useSuspenseQuery(iamGroupsForUserQueryOptions(userName))
  const remove = useRemoveUserFromGroup()
  const [confirm, setConfirm] = useState<string>()
  const groups = data.Groups ?? []

  async function onRemove() {
    if (!confirm) {
      return
    }

    try {
      await remove.mutateAsync({ groupName: confirm, userName })
    } catch {
      // Shown by the banner above.
    } finally {
      setConfirm(undefined)
    }
  }

  return (
    <PanelSection title="Groups">
      <div className="flex flex-col gap-3">
        {remove.error && (
          <ErrorBanner error={remove.error} msg="Failed to remove from group" />
        )}
        {groups.length > 0 ? (
          <IamItems>
            {groups.map((group) => (
              <IamItem
                action={
                  <Button
                    disabled={
                      remove.isPending &&
                      remove.variables?.groupName === group.GroupName
                    }
                    onClick={() => {
                      setConfirm(group.GroupName)
                    }}
                    size="sm"
                    variant="outline"
                  >
                    Remove
                  </Button>
                }
                key={group.GroupName}
              >
                <Link
                  className="text-primary hover:underline"
                  params={{ groupName: group.GroupName ?? "" }}
                  to="/iam/list-groups/$groupName"
                >
                  {group.GroupName}
                </Link>
              </IamItem>
            ))}
          </IamItems>
        ) : (
          <p className="text-sm text-muted-foreground">
            Not a member of any group.
          </p>
        )}
      </div>
      <DeleteConfirmationDialog
        confirmLabel="Remove"
        description={`Remove ${userName} from ${confirm ?? "the group"}? The user loses the permissions the group grants.`}
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
        title="Remove from group"
      />
    </PanelSection>
  )
}

interface KeyAction {
  key: AccessKeyMetadata
  action: "activate" | "deactivate" | "delete"
}

const KEY_ACTION_COPY = {
  activate: {
    title: "Activate access key",
    label: "Activate",
    pending: "Activating…",
    effect: "Requests signed with it are accepted again.",
  },
  deactivate: {
    title: "Deactivate access key",
    label: "Deactivate",
    pending: "Deactivating…",
    effect: "Requests signed with it are refused until it is activated.",
  },
  delete: {
    title: "Delete access key",
    label: "Delete",
    pending: "Deleting…",
    effect: "Requests signed with it are refused. This cannot be undone.",
  },
} as const

function AccessKeys({ userName }: { userName: string }) {
  const { data } = useSuspenseQuery(iamAccessKeysQueryOptions(userName))
  const create = useCreateAccessKey()
  const update = useUpdateAccessKey()
  const remove = useDeleteAccessKey()
  const [confirm, setConfirm] = useState<KeyAction>()

  const [newAccessKey, setNewAccessKey] = useState<{
    accessKeyId: string
    secretAccessKey: string
  } | null>(null)

  const accessKeys = data.AccessKeyMetadata ?? []
  const pending = update.isPending || remove.isPending
  const error = create.error ?? update.error ?? remove.error

  async function onCreate() {
    try {
      const response = await create.mutateAsync(userName)

      if (response.AccessKey) {
        setNewAccessKey({
          accessKeyId: response.AccessKey.AccessKeyId ?? "",
          secretAccessKey: response.AccessKey.SecretAccessKey ?? "",
        })
      }
    } catch {
      // Shown by the banner above.
    }
  }

  async function onConfirm() {
    const accessKeyId = confirm?.key.AccessKeyId

    if (!confirm || !accessKeyId) {
      return
    }

    try {
      await (confirm.action === "delete"
        ? remove.mutateAsync({ userName, accessKeyId })
        : update.mutateAsync({
            userName,
            accessKeyId,
            status: confirm.action === "activate" ? "Active" : "Inactive",
          }))
    } catch {
      // Shown by the banner above.
    } finally {
      setConfirm(undefined)
    }
  }

  const copy = confirm ? KEY_ACTION_COPY[confirm.action] : undefined

  return (
    <PanelSection title="Access keys">
      <div className="flex flex-col gap-3">
        {error && <ErrorBanner error={error} msg="Access key action failed" />}
        <div>
          <Button
            disabled={create.isPending}
            onClick={() => {
              void onCreate()
            }}
            size="sm"
            variant="outline"
          >
            <KeyRound data-icon="inline-start" />
            {create.isPending ? "Creating…" : "Create access key"}
          </Button>
        </div>
        {accessKeys.length > 0 ? (
          <IamItems>
            {accessKeys.map((key) => (
              <IamItem
                action={
                  <>
                    <Button
                      disabled={pending}
                      onClick={() => {
                        setConfirm({
                          key,
                          action:
                            key.Status === "Active" ? "deactivate" : "activate",
                        })
                      }}
                      size="sm"
                      variant="outline"
                    >
                      {key.Status === "Active" ? "Deactivate" : "Activate"}
                    </Button>
                    <Button
                      aria-label={`Delete access key ${key.AccessKeyId ?? ""}`}
                      disabled={pending}
                      onClick={() => {
                        setConfirm({ key, action: "delete" })
                      }}
                      size="icon-sm"
                      variant="destructive"
                    >
                      <Trash2 />
                    </Button>
                  </>
                }
                detail={`Created ${formatDateTime(key.CreateDate)}`}
                key={key.AccessKeyId}
              >
                <span className="flex items-center gap-2">
                  <span className="truncate font-mono text-xs">
                    {key.AccessKeyId}
                  </span>
                  <StateBadge state={key.Status} />
                </span>
              </IamItem>
            ))}
          </IamItems>
        ) : (
          <p className="text-sm text-muted-foreground">No access keys.</p>
        )}
      </div>
      <DeleteConfirmationDialog
        confirmLabel={copy?.label}
        description={`${copy?.label ?? ""} ${confirm?.key.AccessKeyId ?? ""}? ${copy?.effect ?? ""}`}
        isPending={pending}
        onConfirm={() => {
          void onConfirm()
        }}
        onOpenChange={(open) => {
          if (!open) {
            setConfirm(undefined)
          }
        }}
        open={confirm !== undefined}
        pendingLabel={copy?.pending}
        title={copy?.title ?? ""}
      />
      {newAccessKey && (
        <AccessKeyModal
          accessKeyId={newAccessKey.accessKeyId}
          onClose={() => {
            setNewAccessKey(null)
          }}
          secretAccessKey={newAccessKey.secretAccessKey}
        />
      )}
    </PanelSection>
  )
}

// ListUsers carries no tags, so they come from GetUser.
function Tags({ userName }: { userName: string }) {
  const { data } = useSuspenseQuery(iamUserQueryOptions(userName))

  return <PanelTags tags={data.User?.Tags} />
}

function Overview({ user }: { user: User }) {
  const userName = user.UserName ?? ""

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="User name">{user.UserName}</PanelField>
          <PanelField copy={user.UserId} label="User ID" mono>
            {user.UserId}
          </PanelField>
          <PanelField copy={user.Arn} label="ARN" mono>
            {user.Arn}
          </PanelField>
          <PanelField label="Path" mono>
            {user.Path}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(user.CreateDate)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Suspense fallback={<PanelSpinner />}>
        <AttachedPoliciesPanel kind="user" name={userName} />
        <InlinePoliciesPanel kind="user" name={userName} />
        <Groups userName={userName} />
      </Suspense>
    </>
  )
}

function Settings({
  userName,
  onDeleted,
}: {
  userName: string
  onDeleted: () => void
}) {
  const remove = useDeleteUser()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Suspense fallback={<PanelSpinner />}>
        <AccessKeys userName={userName} />
        <Tags userName={userName} />
      </Suspense>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <p className="text-sm text-muted-foreground">
            Detach its policies, delete its access keys and remove it from its
            groups before deleting it.
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
            Delete user
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete the user ${userName}? ${iamDeleteWarning("user")}`}
        identifier={userName}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete user"
      />
    </>
  )
}

interface UserDetailsProps {
  user: User
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One user's details, as the list's side panel or as its own page.
export function UserDetails({
  user,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: UserDetailsProps) {
  const userName = user.UserName ?? ""

  return (
    <DetailPanel
      closeLabel={layout === "page" ? "Back to users" : "Close user details"}
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open user page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ userName }}
            to="/iam/list-users/$userName"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="User details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={user.UserId}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview user={user} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} userName={userName} />,
        },
      ]}
      title={userName}
    />
  )
}
