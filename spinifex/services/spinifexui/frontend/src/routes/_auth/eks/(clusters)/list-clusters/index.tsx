import { createFileRoute } from "@tanstack/react-router"

import { eksClustersQueryOptions } from "@/queries/eks"

import { ClustersListPage } from "../-components/clusters-list-page"

export const Route = createFileRoute("/_auth/eks/(clusters)/list-clusters/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(eksClustersQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Clusters | EKS | Mulga" }],
  }),
  component: ClustersListPage,
})
