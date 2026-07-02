import type { Cluster } from "@aws-sdk/client-ecs"
import { useQueryClient, useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { isEcsSystemImage } from "@/lib/system-managed"
import { useDeleteCluster } from "@/mutations/ecs"
import { ec2ImagesQueryOptions } from "@/queries/ec2"
import { ecsClustersQueryOptions } from "@/queries/ecs"

import { CreateClusterDialog } from "./create-cluster-dialog"
import { EcsSectionNav } from "./ecs-section-nav"
import { EcsSystemImageRequired } from "./ecs-system-image-required"

export function ClustersListPage() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: clusters } = useSuspenseQuery(ecsClustersQueryOptions)
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)
  const deleteCluster = useDeleteCluster()
  const [createOpen, setCreateOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)
  const [isRechecking, setIsRechecking] = useState(false)

  const hasEcsSystemImage = (imagesData.Images ?? []).some(isEcsSystemImage)

  async function handleRecheck() {
    setIsRechecking(true)
    try {
      await queryClient.invalidateQueries({
        queryKey: ec2ImagesQueryOptions.queryKey,
      })
    } finally {
      setIsRechecking(false)
    }
  }

  function handleDelete() {
    if (!deleteTarget) {
      return
    }
    deleteCluster.mutate(deleteTarget, {
      onSuccess: () => setDeleteTarget(null),
    })
  }

  return (
    <>
      <PageHeading
        actions={
          <Button
            disabled={!hasEcsSystemImage}
            onClick={() => setCreateOpen(true)}
          >
            Create Cluster
          </Button>
        }
        title="Clusters"
      />

      <EcsSectionNav />

      {!hasEcsSystemImage && (
        <EcsSystemImageRequired
          isRechecking={isRechecking}
          onRecheck={handleRecheck}
        />
      )}

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

      {clusters.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">Status</th>
                <th className="px-4 py-2 font-medium">Services</th>
                <th className="px-4 py-2 font-medium">Running Tasks</th>
                <th className="px-4 py-2 font-medium">Instances</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {clusters.map((cluster: Cluster) => {
                if (!cluster.clusterName) {
                  return null
                }
                const name = cluster.clusterName
                return (
                  <tr
                    className="cursor-pointer border-b transition-colors last:border-0 hover:bg-accent"
                    key={name}
                    onClick={async () =>
                      await navigate({
                        to: "/ecs/list-clusters/$clusterName",
                        params: { clusterName: name },
                      })
                    }
                  >
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                        params={{ clusterName: name }}
                        to="/ecs/list-clusters/$clusterName"
                      >
                        {name}
                      </Link>
                    </td>
                    <td className="px-4 py-2">
                      <StateBadge state={cluster.status} />
                    </td>
                    <td className="px-4 py-2">
                      {cluster.activeServicesCount ?? 0}
                    </td>
                    <td className="px-4 py-2">
                      {cluster.runningTasksCount ?? 0}
                    </td>
                    <td className="px-4 py-2">
                      {cluster.registeredContainerInstancesCount ?? 0}
                    </td>
                    <td className="px-4 py-2 text-right">
                      <Button
                        onClick={(e) => {
                          e.stopPropagation()
                          setDeleteTarget(name)
                        }}
                        size="sm"
                        variant="destructive"
                      >
                        Delete
                      </Button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="text-muted-foreground">No ECS clusters found.</p>
      )}

      <CreateClusterDialog onOpenChange={setCreateOpen} open={createOpen} />

      <DeleteConfirmationDialog
        description={`This permanently deletes cluster "${deleteTarget}" and force-stops every task it holds. This cannot be undone.`}
        isPending={deleteCluster.isPending}
        onConfirm={handleDelete}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        open={deleteTarget !== null}
        title="Delete cluster"
      />
    </>
  )
}
