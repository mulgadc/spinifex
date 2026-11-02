import type { Listener } from "@aws-sdk/client-elastic-load-balancing-v2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Pencil, Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelField, PanelFields } from "@/components/detail-panel"
import { ListenerEditForm } from "@/components/elbv2/listener-edit-form"
import { ListenerRulesTab } from "@/components/elbv2/listener-rules-tab"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { arnResourceId } from "@/lib/utils"
import {
  useCreateListener,
  useDeleteListener,
  useModifyListener,
} from "@/mutations/elbv2"
import { acmCertificatesQueryOptions } from "@/queries/acm"
import {
  elbv2ListenersQueryOptions,
  elbv2SslPoliciesQueryOptions,
  elbv2TargetGroupsQueryOptions,
} from "@/queries/elbv2"
import {
  type CreateListenerFormData,
  type LbType,
  protocolsForType,
} from "@/types/elbv2"

interface ListenersTabProps {
  loadBalancerArn: string
  vpcId: string | undefined
  lbType: LbType
}

function formatTls(listener: Listener): string {
  if (listener.Protocol !== "HTTPS") {
    return "—"
  }

  const certId = arnResourceId(listener.Certificates?.[0]?.CertificateArn)
  const policy = listener.SslPolicy

  if (certId && policy) {
    return `${policy} · ${certId}`
  }

  return policy ?? (certId || "—")
}

export function ListenersTab({
  loadBalancerArn,
  vpcId,
  lbType,
}: ListenersTabProps) {
  const protocols = protocolsForType(lbType)

  const { data: listenersData } = useSuspenseQuery(
    elbv2ListenersQueryOptions(loadBalancerArn),
  )

  const { data: tgsData } = useSuspenseQuery(elbv2TargetGroupsQueryOptions)
  const { data: certsData } = useSuspenseQuery(acmCertificatesQueryOptions)

  const { data: sslPoliciesData } = useSuspenseQuery(
    elbv2SslPoliciesQueryOptions,
  )

  const createMutation = useCreateListener()
  const deleteMutation = useDeleteListener()
  const modifyMutation = useModifyListener()

  const [addOpen, setAddOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<Listener | undefined>()
  const [deleteTarget, setDeleteTarget] = useState<Listener | undefined>()
  const [rulesOpenArn, setRulesOpenArn] = useState<string | undefined>()

  const listeners = listenersData.Listeners ?? []
  const allTgs = tgsData.TargetGroups ?? []
  const vpcTgs = allTgs.filter((tg) => tg.VpcId === vpcId)
  const certificates = certsData.CertificateSummaryList ?? []
  const sslPolicies = sslPoliciesData.SslPolicies ?? []

  const tgNameByArn = new Map<string, string>()

  for (const tg of allTgs) {
    if (tg.TargetGroupArn && tg.TargetGroupName) {
      tgNameByArn.set(tg.TargetGroupArn, tg.TargetGroupName)
    }
  }

  const formatDefaultAction = (listener: Listener): string => {
    const action = listener.DefaultActions?.[0]

    if (!action) {
      return "—"
    }

    if (action.Type === "forward" && action.TargetGroupArn) {
      const name = tgNameByArn.get(action.TargetGroupArn)

      return name ? `forward → ${name}` : `forward → ${action.TargetGroupArn}`
    }

    return action.Type ?? "—"
  }

  const handleCreate = async (data: CreateListenerFormData) => {
    try {
      await createMutation.mutateAsync({
        loadBalancerArn,
        protocol: data.protocol,
        port: data.port,
        defaultTargetGroupArn: data.defaultTargetGroupArn,
        certificateArn: data.certificateArn,
        sslPolicy: data.sslPolicy,
      })
      setAddOpen(false)
    } catch {
      // surfaced via mutation.error
    }
  }

  const handleEdit = async (data: CreateListenerFormData) => {
    if (!editTarget?.ListenerArn) {
      return
    }

    try {
      await modifyMutation.mutateAsync({
        listenerArn: editTarget.ListenerArn,
        loadBalancerArn,
        protocol: data.protocol,
        port: data.port,
        defaultTargetGroupArn: data.defaultTargetGroupArn,
        certificateArn: data.certificateArn,
        sslPolicy: data.sslPolicy,
      })
      setEditTarget(undefined)
    } catch {
      // surfaced via mutation.error
    }
  }

  const handleDelete = async () => {
    if (!deleteTarget?.ListenerArn) {
      return
    }

    try {
      await deleteMutation.mutateAsync(deleteTarget.ListenerArn)
    } finally {
      setDeleteTarget(undefined)
    }
  }

  return (
    <div className="space-y-3">
      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete listener"
        />
      )}

      <div className="flex items-center justify-between">
        <p className="text-xs text-muted-foreground">
          {listeners.length} listener{listeners.length === 1 ? "" : "s"}
        </p>
        <Button
          onClick={() => {
            setAddOpen(true)
          }}
          size="sm"
        >
          Add listener
        </Button>
      </div>

      {listeners.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No listeners configured.
        </p>
      ) : (
        <ul className="flex flex-col gap-2">
          {listeners.map((listener) => {
            const arn = listener.ListenerArn ?? ""
            const rulesOpen = rulesOpenArn === arn
            const name = `${listener.Protocol}:${listener.Port}`

            return (
              <li className="rounded-md border bg-card" key={arn}>
                <div className="flex items-center justify-between gap-2 py-1 pr-1 pl-3">
                  <span className="text-sm font-medium">{name}</span>
                  <div className="flex gap-1">
                    <Button
                      aria-label={`${rulesOpen ? "Hide" : "Show"} rules for listener ${name}`}
                      onClick={() => {
                        setRulesOpenArn(rulesOpen ? undefined : arn)
                      }}
                      size="sm"
                      variant="ghost"
                    >
                      {rulesOpen ? "Hide rules" : "Rules"}
                    </Button>
                    <Button
                      aria-label={`Edit listener ${name}`}
                      onClick={() => {
                        setEditTarget(listener)
                      }}
                      size="icon-sm"
                      variant="ghost"
                    >
                      <Pencil />
                    </Button>
                    <Button
                      aria-label={`Delete listener ${name}`}
                      onClick={() => {
                        setDeleteTarget(listener)
                      }}
                      size="icon-sm"
                      variant="ghost"
                    >
                      <Trash2 />
                    </Button>
                  </div>
                </div>
                <div className="border-t px-3">
                  <PanelFields>
                    {listener.Protocol === "HTTPS" && (
                      <PanelField label="TLS">{formatTls(listener)}</PanelField>
                    )}
                    <PanelField label="Default action">
                      {formatDefaultAction(listener)}
                    </PanelField>
                    <PanelField copy={arn} label="Listener ARN" mono>
                      {arn}
                    </PanelField>
                  </PanelFields>
                </div>
                {rulesOpen && arn && (
                  <div className="border-t bg-muted/30 p-3">
                    <ListenerRulesTab listenerArn={arn} targetGroups={vpcTgs} />
                  </div>
                )}
              </li>
            )
          })}
        </ul>
      )}

      {/* Mounted only while open so every session starts from fresh values. */}
      {addOpen && (
        <ListenerEditForm
          certificates={certificates}
          error={createMutation.error}
          isPending={createMutation.isPending}
          lbType={lbType}
          loadBalancerArn={loadBalancerArn}
          onClose={() => {
            createMutation.reset()
            setAddOpen(false)
          }}
          onSubmit={handleCreate}
          protocols={protocols}
          sslPolicies={sslPolicies}
          targetGroups={vpcTgs}
        />
      )}

      {editTarget && (
        <ListenerEditForm
          certificates={certificates}
          error={modifyMutation.error}
          isPending={modifyMutation.isPending}
          key={editTarget.ListenerArn}
          lbType={lbType}
          listener={editTarget}
          loadBalancerArn={loadBalancerArn}
          onClose={() => {
            modifyMutation.reset()
            setEditTarget(undefined)
          }}
          onSubmit={handleEdit}
          protocols={protocols}
          sslPolicies={sslPolicies}
          targetGroups={vpcTgs}
        />
      )}

      <DeleteConfirmationDialog
        description={
          deleteTarget
            ? `Delete listener ${deleteTarget.Protocol}:${deleteTarget.Port}? This cannot be undone.`
            : ""
        }
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(undefined)
          }
        }}
        open={deleteTarget !== undefined}
        title="Delete listener"
      />
    </div>
  )
}
