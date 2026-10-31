import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Plus, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { StateBadge } from "@/components/state-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { useDeleteNodegroup } from "@/mutations/eks"
import {
  eksNodegroupQueryOptions,
  eksNodegroupsQueryOptions,
} from "@/queries/eks"

import { AddNodegroupForm } from "./add-nodegroup-form"
import { ScaleNodegroupForm } from "./scale-nodegroup-form"

// GPU AMI types per the AWS EKS AMITypes enum (AL2_x86_64_GPU,
// BOTTLEROCKET_x86_64_NVIDIA, BOTTLEROCKET_ARM_64_NVIDIA). DescribeNodegroup
// forces amiType to the GPU variant whenever the nodegroup is GPU-enabled, so
// an already-created nodegroup's GPU badge reads that response field instead
// of re-deriving it from instance types.
function isGpuAmiType(amiType: string | undefined): boolean {
  return !!amiType && (amiType.endsWith("_GPU") || amiType.endsWith("_NVIDIA"))
}

function NodegroupRow({
  clusterName,
  clusterVersion,
  nodegroupName,
  onDelete,
}: {
  clusterName: string
  clusterVersion: string | undefined
  nodegroupName: string
  onDelete: (name: string) => void
}) {
  const { data } = useQuery(
    eksNodegroupQueryOptions(clusterName, nodegroupName),
  )

  const ng = data?.nodegroup
  const scaling = ng?.scalingConfig
  const [showScale, setShowScale] = useState(false)

  const updateAvailable =
    !!clusterVersion && !!ng?.version && ng.version !== clusterVersion

  const isGpu = isGpuAmiType(ng?.amiType)

  return (
    <div className="rounded-md border px-3 py-2">
      <div className="flex items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate text-sm font-medium">{nodegroupName}</span>
          {isGpu && <Badge variant="secondary">GPU</Badge>}
        </div>
        <div className="flex items-center gap-1">
          <Button
            aria-label="Scale node group"
            onClick={() => {
              setShowScale(true)
            }}
            size="icon-sm"
            variant="ghost"
          >
            <SlidersHorizontal />
          </Button>
          <Button
            aria-label="Delete node group"
            onClick={() => {
              onDelete(nodegroupName)
            }}
            size="icon-sm"
            variant="ghost"
          >
            <Trash2 />
          </Button>
        </div>
      </div>
      <PanelFields>
        <PanelField label="Status">
          {ng?.status && <StateBadge state={ng.status} />}
        </PanelField>
        <PanelField label="Kubernetes version">
          {updateAvailable
            ? `${ng?.version} (cluster on ${clusterVersion})`
            : ng?.version}
        </PanelField>
        <PanelField label="Instance types" mono>
          {ng?.instanceTypes?.join(", ")}
        </PanelField>
        <PanelField label="AMI type" mono>
          {ng?.amiType}
        </PanelField>
        <PanelField label="Capacity type">{ng?.capacityType}</PanelField>
        <PanelField label="Scaling (min / desired / max)">
          {scaling
            ? `${scaling.minSize} / ${scaling.desiredSize} / ${scaling.maxSize}`
            : undefined}
        </PanelField>
      </PanelFields>

      {showScale && (
        <ScaleNodegroupForm
          clusterName={clusterName}
          current={scaling ?? {}}
          nodegroupName={nodegroupName}
          onClose={() => {
            setShowScale(false)
          }}
        />
      )}
    </div>
  )
}

export function NodegroupsTab({
  clusterName,
  clusterVersion,
  vpcId,
}: {
  clusterName: string
  clusterVersion: string | undefined
  vpcId: string | undefined
}) {
  const { data } = useSuspenseQuery(eksNodegroupsQueryOptions(clusterName))
  const deleteNodegroup = useDeleteNodegroup()
  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<string | undefined>()

  const nodegroups = data.nodegroups ?? []

  return (
    <>
      <PanelSection title={`Node groups (${nodegroups.length})`}>
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
              Add node group
            </Button>
          </div>
          {nodegroups.length > 0 ? (
            nodegroups.map((ng) => (
              <NodegroupRow
                clusterName={clusterName}
                clusterVersion={clusterVersion}
                key={ng}
                nodegroupName={ng}
                onDelete={setPendingDelete}
              />
            ))
          ) : (
            <p className="text-sm text-muted-foreground">No node groups.</p>
          )}
        </div>
      </PanelSection>

      {showAdd && (
        <AddNodegroupForm
          clusterName={clusterName}
          onClose={() => {
            setShowAdd(false)
          }}
          vpcId={vpcId}
        />
      )}

      <DeleteConfirmationDialog
        description={`Delete node group "${pendingDelete ?? ""}"? This terminates its nodes.`}
        isPending={deleteNodegroup.isPending}
        onConfirm={() => {
          if (!pendingDelete) {
            return
          }

          deleteNodegroup.mutate(
            { clusterName, nodegroupName: pendingDelete },
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
        title="Delete node group"
      />
    </>
  )
}
