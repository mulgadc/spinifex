import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { useDeleteCluster } from "@/mutations/ecs"
import { ecsClusterQueryOptions } from "@/queries/ecs"

import { ContainerInstancesTab } from "./container-instances-tab"
import { ServicesTab } from "./services-tab"
import { TaskDefinitionsTab } from "./task-definitions-tab"
import { TasksTab } from "./tasks-tab"

export function ClusterDetailPage({ clusterName }: { clusterName: string }) {
  const navigate = useNavigate()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const { data: cluster } = useSuspenseQuery(
    ecsClusterQueryOptions(clusterName),
  )

  const deleteCluster = useDeleteCluster()

  function handleDelete() {
    deleteCluster.mutate(clusterName, {
      onSuccess: () => {
        void navigate({ to: "/ecs/list-clusters" })
      },
    })
  }

  return (
    <>
      <BackLink to="/ecs/list-clusters">Clusters</BackLink>

      <PageHeading
        actions={
          <div className="flex items-center gap-2">
            <Button
              onClick={() => setShowDeleteDialog(true)}
              size="sm"
              variant="destructive"
            >
              <Trash2 className="size-4" />
              Delete
            </Button>
            <StateBadge state={cluster?.status} />
          </div>
        }
        subtitle="Cluster Details"
        title={clusterName}
      />

      {deleteCluster.isError && (
        <ErrorBanner
          error={
            deleteCluster.error instanceof Error
              ? deleteCluster.error
              : undefined
          }
          msg="Failed to delete cluster."
        />
      )}

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTab value="overview">Overview</TabsTab>
          <TabsTab value="services">Services</TabsTab>
          <TabsTab value="tasks">Tasks</TabsTab>
          <TabsTab value="task-definitions">Task Definitions</TabsTab>
          <TabsTab value="instances">Container Instances</TabsTab>
        </TabsList>

        <TabsPanel value="overview">
          <DetailCard>
            <DetailCard.Header>Cluster</DetailCard.Header>
            <DetailCard.Content>
              <DetailRow label="ARN" value={cluster?.clusterArn} />
              <DetailRow label="Status" value={cluster?.status} />
              <DetailRow
                label="Active services"
                value={String(cluster?.activeServicesCount ?? 0)}
              />
              <DetailRow
                label="Running tasks"
                value={String(cluster?.runningTasksCount ?? 0)}
              />
              <DetailRow
                label="Pending tasks"
                value={String(cluster?.pendingTasksCount ?? 0)}
              />
              <DetailRow
                label="Registered instances"
                value={String(cluster?.registeredContainerInstancesCount ?? 0)}
              />
            </DetailCard.Content>
          </DetailCard>
        </TabsPanel>

        <TabsPanel value="services">
          <ServicesTab clusterName={clusterName} />
        </TabsPanel>

        <TabsPanel value="tasks">
          <TasksTab clusterName={clusterName} />
        </TabsPanel>

        <TabsPanel value="task-definitions">
          <TaskDefinitionsTab clusterName={clusterName} />
        </TabsPanel>

        <TabsPanel value="instances">
          <ContainerInstancesTab clusterName={clusterName} />
        </TabsPanel>
      </Tabs>

      <DeleteConfirmationDialog
        description={`This permanently deletes cluster "${clusterName}" and force-stops every task it holds. This cannot be undone.`}
        isPending={deleteCluster.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete cluster"
      />
    </>
  )
}
