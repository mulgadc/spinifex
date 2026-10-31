import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Plus, Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { arnResourceId } from "@/lib/utils"
import {
  useAssociateAccessPolicy,
  useDeleteAccessEntry,
  useDisassociateAccessPolicy,
} from "@/mutations/eks"
import {
  eksAccessEntriesQueryOptions,
  eksAccessEntryQueryOptions,
  eksAssociatedAccessPoliciesQueryOptions,
} from "@/queries/eks"
import { EKS_ACCESS_POLICIES } from "@/types/eks"

import { AddAccessEntryForm } from "./add-access-entry-form"

const ACCESS_POLICY_ITEMS = EKS_ACCESS_POLICIES.map((p) => ({
  value: p.arn,
  label: p.name,
}))

function AccessEntryRow({
  clusterName,
  principalArn,
  onDelete,
}: {
  clusterName: string
  principalArn: string
  onDelete: (arn: string) => void
}) {
  const { data: entryData } = useQuery(
    eksAccessEntryQueryOptions(clusterName, principalArn),
  )

  const { data: policyData } = useQuery(
    eksAssociatedAccessPoliciesQueryOptions(clusterName, principalArn),
  )

  const associate = useAssociateAccessPolicy()
  const disassociate = useDisassociateAccessPolicy()
  const [policyArn, setPolicyArn] = useState<string>(EKS_ACCESS_POLICIES[0].arn)

  const entry = entryData?.accessEntry
  const associated = policyData?.associatedAccessPolicies ?? []

  return (
    <div className="rounded-md border px-3 py-2">
      <div className="flex items-center justify-between gap-2">
        <span className="min-w-0 truncate font-mono text-xs">
          {principalArn}
        </span>
        <Button
          aria-label="Delete access entry"
          onClick={() => {
            onDelete(principalArn)
          }}
          size="icon-sm"
          variant="ghost"
        >
          <Trash2 />
        </Button>
      </div>
      <PanelFields>
        <PanelField label="Type">{entry?.type}</PanelField>
        <PanelField label="Kubernetes groups">
          {entry?.kubernetesGroups?.length
            ? entry.kubernetesGroups.join(", ")
            : undefined}
        </PanelField>
        {associated.length > 0 ? (
          associated.map((p, index) => (
            <div
              className="flex items-center justify-between gap-4 py-1.5 text-sm"
              key={p.policyArn}
            >
              <span className="shrink-0 text-muted-foreground">
                {index === 0 ? "Access policies" : ""}
              </span>
              <span className="flex min-w-0 items-center gap-1">
                <span className="truncate">{arnResourceId(p.policyArn)}</span>
                <Button
                  onClick={() => {
                    disassociate.mutate({
                      clusterName,
                      principalArn,
                      policyArn: p.policyArn ?? "",
                    })
                  }}
                  size="xs"
                  variant="ghost"
                >
                  Remove
                </Button>
              </span>
            </div>
          ))
        ) : (
          <PanelField label="Access policies" />
        )}
      </PanelFields>
      <div className="flex items-center gap-2 pt-2">
        <Select
          items={ACCESS_POLICY_ITEMS}
          onValueChange={(next: string | null) => {
            if (next) {
              setPolicyArn(next)
            }
          }}
          value={policyArn}
        >
          <SelectTrigger aria-label="Access policy" className="min-w-0 flex-1">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {ACCESS_POLICY_ITEMS.map((p) => (
              <SelectItem key={p.value} value={p.value}>
                {p.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button
          disabled={associate.isPending}
          onClick={() => {
            associate.mutate({
              clusterName,
              principalArn,
              policyArn,
              accessScopeType: "cluster",
            })
          }}
          size="sm"
        >
          Associate
        </Button>
      </div>
    </div>
  )
}

export function AccessTab({ clusterName }: { clusterName: string }) {
  const { data } = useSuspenseQuery(eksAccessEntriesQueryOptions(clusterName))
  const deleteEntry = useDeleteAccessEntry()
  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<string | undefined>()

  const entries = data.accessEntries ?? []

  return (
    <>
      <PanelSection title={`Access entries (${entries.length})`}>
        <div className="flex flex-col gap-3 py-1">
          <div>
            <Button
              onClick={() => {
                setShowAdd(true)
              }}
              size="sm"
              variant="outline"
            >
              <Plus data-icon="inline-start" />
              Add access entry
            </Button>
          </div>
          {entries.length > 0 ? (
            entries.map((arn) => (
              <AccessEntryRow
                clusterName={clusterName}
                key={arn}
                onDelete={setPendingDelete}
                principalArn={arn}
              />
            ))
          ) : (
            <p className="text-sm text-muted-foreground">No access entries.</p>
          )}
        </div>
      </PanelSection>

      {showAdd && (
        <AddAccessEntryForm
          clusterName={clusterName}
          onClose={() => {
            setShowAdd(false)
          }}
        />
      )}

      <DeleteConfirmationDialog
        description={`Delete access entry for "${pendingDelete ?? ""}"?`}
        isPending={deleteEntry.isPending}
        onConfirm={() => {
          if (!pendingDelete) {
            return
          }

          deleteEntry.mutate(
            { clusterName, principalArn: pendingDelete },
            {
              onSuccess: () => {
                setPendingDelete(undefined)
              },
            },
          )
        }}
        onOpenChange={(o) => {
          if (!o) {
            setPendingDelete(undefined)
          }
        }}
        open={pendingDelete !== undefined}
        title="Delete access entry"
      />
    </>
  )
}
