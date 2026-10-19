import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { eksClustersQueryOptions } from "@/queries/eks"

export function ClustersListPage() {
  const { data } = useSuspenseQuery(eksClustersQueryOptions)

  const clusters = (data.clusters ?? []).toSorted(byName((name) => name))

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

      <ResourceList
        empty="No EKS clusters found."
        getKey={(clusterName) => clusterName}
        items={clusters}
        renderItem={(clusterName) => (
          <ListCard
            params={{ clusterName }}
            title={clusterName}
            to="/eks/list-clusters/$clusterName"
          />
        )}
      />
    </>
  )
}
