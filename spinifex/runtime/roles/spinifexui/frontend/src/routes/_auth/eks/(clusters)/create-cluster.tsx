import { createFileRoute } from "@tanstack/react-router"

import {
  ec2ImagesQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { iamRolesQueryOptions } from "@/queries/iam"

import { CreateClusterPage } from "./-components/create-cluster-page"

export const Route = createFileRoute("/_auth/eks/(clusters)/create-cluster")({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2VpcsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2SubnetsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...iamRolesQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2ImagesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: () => ({
    meta: [{ title: "Create Cluster | EKS | Mulga" }],
  }),
  component: CreateClusterPage,
})
