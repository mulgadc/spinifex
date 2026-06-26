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

  if (!cluster) {
    return (
      <>
        <BackLink to="/ecs/list-clusters">Clusters</BackLink>
        <PageHeading subtitle="Cluster Details" title={clusterName} />
        <p className="text-muted-foreground">Cluster not found.</p>
      </>
    )
  }

  const tags = cluster.tags ?? []

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
            <StateBadge state={cluster.status} />
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

      <DetailCard>
        <DetailCard.Header>Overview</DetailCard.Header>
        <DetailCard.Content>
          <DetailRow label="ARN" value={cluster.clusterArn} />
          <DetailRow label="Status" value={cluster.status} />
          <DetailRow
            label="Active services"
            value={String(cluster.activeServicesCount ?? 0)}
          />
          <DetailRow
            label="Running tasks"
            value={String(cluster.runningTasksCount ?? 0)}
          />
          <DetailRow
            label="Pending tasks"
            value={String(cluster.pendingTasksCount ?? 0)}
          />
          <DetailRow
            label="Registered instances"
            value={String(cluster.registeredContainerInstancesCount ?? 0)}
          />
        </DetailCard.Content>
      </DetailCard>

      <Tabs className="mt-6" defaultValue="services">
        <TabsList>
          <TabsTab value="services">Services</TabsTab>
          <TabsTab value="tasks">Tasks</TabsTab>
          <TabsTab value="infrastructure">Infrastructure</TabsTab>
          <TabsTab value="tags">Tags</TabsTab>
        </TabsList>

        <TabsPanel value="services">
          <ServicesTab clusterName={clusterName} />
        </TabsPanel>

        <TabsPanel value="tasks">
          <TasksTab clusterName={clusterName} />
        </TabsPanel>

        <TabsPanel value="infrastructure">
          <ContainerInstancesTab clusterName={clusterName} />
        </TabsPanel>

        <TabsPanel value="tags">
          {tags.length === 0 ? (
            <p className="text-muted-foreground">No tags.</p>
          ) : (
            <div className="overflow-x-auto rounded-lg border bg-card">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b text-left text-muted-foreground">
                    <th className="px-4 py-2 font-medium">Key</th>
                    <th className="px-4 py-2 font-medium">Value</th>
                  </tr>
                </thead>
                <tbody>
                  {tags.map((tag) => (
                    <tr className="border-b last:border-0" key={tag.key}>
                      <td className="px-4 py-2 font-mono text-xs">{tag.key}</td>
                      <td className="px-4 py-2 font-mono text-xs">
                        {tag.value}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
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
