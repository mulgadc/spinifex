import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { eksClustersQueryOptions } from "@/queries/eks"

export function ClustersListPage() {
  const { data } = useSuspenseQuery(eksClustersQueryOptions)

  const clusters = (data.clusters ?? []).toSorted((a, b) =>
    a.toLowerCase().localeCompare(b.toLowerCase()),
  )

  return (
    <>
      <PageHeading
        actions={
          <Link to="/eks/create-cluster">
            <Button>Create Cluster</Button>
          </Link>
        }
        title="Clusters"
      />

      {clusters.length > 0 ? (
        <div className="space-y-4">
          {clusters.map((clusterName) => (
            <ListCard
              key={clusterName}
              params={{ clusterName }}
              title={clusterName}
              to="/eks/list-clusters/$clusterName"
            />
          ))}
        </div>
      ) : (
        <p className="text-muted-foreground">No EKS clusters found.</p>
      )}
    </>
  )
}
