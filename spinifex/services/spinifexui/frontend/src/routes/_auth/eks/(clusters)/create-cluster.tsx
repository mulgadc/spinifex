import { createFileRoute } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { PageHeading } from "@/components/page-heading"

export const Route = createFileRoute("/_auth/eks/(clusters)/create-cluster")({
  head: () => ({
    meta: [{ title: "Create Cluster | EKS | Mulga" }],
  }),
  component: CreateCluster,
})

function CreateCluster() {
  return (
    <>
      <BackLink to="/eks/list-clusters">Clusters</BackLink>
      <PageHeading subtitle="EKS" title="Create Cluster" />
      <p className="text-muted-foreground">
        Cluster creation wizard is coming soon.
      </p>
    </>
  )
}
