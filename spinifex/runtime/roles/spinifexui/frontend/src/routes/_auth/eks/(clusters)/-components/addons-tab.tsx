import type { AddonInfo } from "@aws-sdk/client-eks"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { ArrowUpCircle, Plus, Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { useDeleteAddon } from "@/mutations/eks"
import {
  eksAddonQueryOptions,
  eksAddonsQueryOptions,
  eksAddonVersionsQueryOptions,
} from "@/queries/eks"
import { addonStatusVariant } from "@/types/eks"

import { AddAddonForm } from "./add-addon-form"
import { UpdateAddonForm } from "./update-addon-form"

const IN_PROGRESS_STATUSES = new Set(["CREATING", "UPDATING", "DELETING"])
const STATUS_POLL_MS = 5000

function AddonRow({
  clusterName,
  addonName,
  catalog,
  onDelete,
}: {
  clusterName: string
  addonName: string
  catalog: AddonInfo[]
  onDelete: (name: string) => void
}) {
  const { data } = useQuery({
    ...eksAddonQueryOptions(clusterName, addonName),
    refetchInterval: (query) =>
      IN_PROGRESS_STATUSES.has(query.state.data?.addon?.status ?? "")
        ? STATUS_POLL_MS
        : false,
  })

  const [showUpdate, setShowUpdate] = useState(false)

  const addon = data?.addon
  const issue = addon?.health?.issues?.[0]?.message

  const versions =
    catalog
      .find((a) => a.addonName === addonName)
      ?.addonVersions?.map((v) => v.addonVersion ?? "")
      .filter(Boolean) ?? []

  return (
    <div className="rounded-md border px-3 py-2">
      <div className="flex items-center justify-between gap-2">
        <span className="min-w-0 truncate text-sm font-medium">
          {addonName}
        </span>
        <div className="flex items-center gap-1">
          <Badge variant={addonStatusVariant(addon?.status)}>
            {addon?.status ?? "UNKNOWN"}
          </Badge>
          <Button
            aria-label="Update add-on"
            onClick={() => {
              setShowUpdate(true)
            }}
            size="icon-sm"
            variant="ghost"
          >
            <ArrowUpCircle />
          </Button>
          <Button
            aria-label="Remove add-on"
            onClick={() => {
              onDelete(addonName)
            }}
            size="icon-sm"
            variant="ghost"
          >
            <Trash2 />
          </Button>
        </div>
      </div>
      <PanelFields>
        <PanelField label="Version" mono>
          {addon?.addonVersion}
        </PanelField>
        <PanelField copy={addon?.serviceAccountRoleArn} label="IRSA role" mono>
          {addon?.serviceAccountRoleArn}
        </PanelField>
        {issue && (
          <PanelField label="Issue">
            <span className="text-destructive">{issue}</span>
          </PanelField>
        )}
      </PanelFields>

      {showUpdate && (
        <UpdateAddonForm
          addonName={addonName}
          clusterName={clusterName}
          current={{
            addonVersion: addon?.addonVersion,
            serviceAccountRoleArn: addon?.serviceAccountRoleArn,
            configurationValues: addon?.configurationValues,
          }}
          onClose={() => {
            setShowUpdate(false)
          }}
          versions={versions}
        />
      )}
    </div>
  )
}

export function AddonsTab({ clusterName }: { clusterName: string }) {
  const { data } = useSuspenseQuery(eksAddonsQueryOptions(clusterName))
  const { data: catalogData } = useQuery(eksAddonVersionsQueryOptions)
  const deleteAddon = useDeleteAddon()
  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<string | undefined>()

  const addons = data.addons ?? []
  const catalog = catalogData?.addons ?? []

  return (
    <>
      <PanelSection title={`Add-ons (${addons.length})`}>
        <div className="flex flex-col gap-3 py-1">
          <p className="text-xs text-muted-foreground">
            Add-on manifests are staged; activation completes when the cluster
            reports the workload ready.
          </p>
          <div>
            <Button
              onClick={() => {
                setShowAdd(true)
              }}
              size="sm"
              variant="outline"
            >
              <Plus data-icon="inline-start" />
              Add add-on
            </Button>
          </div>
          {addons.length > 0 ? (
            addons.map((name) => (
              <AddonRow
                addonName={name}
                catalog={catalog}
                clusterName={clusterName}
                key={name}
                onDelete={setPendingDelete}
              />
            ))
          ) : (
            <p className="text-sm text-muted-foreground">
              No add-ons installed.
            </p>
          )}
        </div>
      </PanelSection>

      {showAdd && (
        <AddAddonForm
          catalog={catalog}
          clusterName={clusterName}
          installed={addons}
          onClose={() => {
            setShowAdd(false)
          }}
        />
      )}

      <DeleteConfirmationDialog
        confirmLabel="Remove"
        description={`Remove add-on "${pendingDelete ?? ""}" from the cluster?`}
        isPending={deleteAddon.isPending}
        onConfirm={() => {
          if (!pendingDelete) {
            return
          }

          deleteAddon.mutate(
            { clusterName, addonName: pendingDelete },
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
        pendingLabel="Removing…"
        title="Remove add-on"
      />
    </>
  )
}
