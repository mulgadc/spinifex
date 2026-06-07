import { createFileRoute } from "@tanstack/react-router"

import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { iamRolesQueryOptions } from "@/queries/iam"

import { CreateClusterPage } from "./-components/create-cluster-page"

export const Route = createFileRoute("/_auth/eks/(clusters)/create-cluster")({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2VpcsQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
      context.queryClient.ensureQueryData(iamRolesQueryOptions),
    ])
  },
  head: () => ({
    meta: [{ title: "Create Cluster | EKS | Mulga" }],
  }),
  component: CreateClusterPage,
})
