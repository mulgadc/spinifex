import type { Cluster } from "@aws-sdk/client-ecs"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
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
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { useDeleteCluster } from "@/mutations/ecs"

import { ClusterContainerInstances } from "./cluster-container-instances"
import { ClusterServices } from "./cluster-services"
import { ClusterTasks } from "./cluster-tasks"

function Overview({ cluster }: { cluster: Cluster }) {
  const name = cluster.clusterName ?? ""

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={cluster.clusterArn} label="ARN" mono>
            {cluster.clusterArn}
          </PanelField>
          <PanelField label="Status">{cluster.status}</PanelField>
          <PanelField label="Active services">
            {cluster.activeServicesCount ?? 0}
          </PanelField>
          <PanelField label="Running tasks">
            {cluster.runningTasksCount ?? 0}
          </PanelField>
          <PanelField label="Pending tasks">
            {cluster.pendingTasksCount ?? 0}
          </PanelField>
          <PanelField label="Registered instances">
            {cluster.registeredContainerInstancesCount ?? 0}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Suspense fallback={<PanelSpinner />}>
        <ClusterServices clusterName={name} />
        <ClusterTasks clusterName={name} />
        <ClusterContainerInstances clusterName={name} />
      </Suspense>
    </>
  )
}

function Settings({
  cluster,
  onDeleted,
}: {
  cluster: Cluster
  onDeleted: () => void
}) {
  const name = cluster.clusterName ?? ""
  const remove = useDeleteCluster()
  const [confirm, setConfirm] = useState(false)

  // ECS spells its tag fields in lower case; the panel reads EC2's.
  const tags = cluster.tags?.map((tag) => ({ Key: tag.key, Value: tag.value }))

  return (
    <>
      <PanelTags tags={tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {remove.isError && (
            <ErrorBanner error={remove.error} msg="Failed to delete cluster." />
          )}
          <Button
            disabled={remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete cluster
          </Button>
        </div>
      </PanelSection>
      <DeleteConfirmationDialog
        description={`This permanently deletes cluster "${name}" and force-stops every task it holds. This cannot be undone.`}
        isPending={remove.isPending}
        onConfirm={() => {
          remove.mutate(name, {
            onSuccess: () => {
              setConfirm(false)
              onDeleted()
            },
          })
        }}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete cluster"
      />
    </>
  )
}

interface ClusterDetailsProps {
  cluster: Cluster
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One ECS cluster's details, as the list's side panel or as its own page.
export function ClusterDetails({
  cluster,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: ClusterDetailsProps) {
  const name = cluster.clusterName ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to clusters" : "Close cluster details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open cluster page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ clusterName: name }}
            search={{ tab }}
            to="/ecs/list-clusters/$clusterName"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Cluster details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      summary={<StateBadge state={cluster.status} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview cluster={cluster} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings cluster={cluster} onDeleted={onClose} />,
        },
      ]}
      title={name}
    />
  )
}
