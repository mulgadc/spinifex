import { createFileRoute } from "@tanstack/react-router"

import {
  ec2ImagesQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import {
  rdsEngineVersionsQueryOptions,
  rdsParameterGroupsQueryOptions,
  rdsSubnetGroupsQueryOptions,
} from "@/queries/rds"

import { CreateDBInstancePage } from "./-components/create-db-instance-page"

export const Route = createFileRoute(
  "/_auth/rds/(db-instances)/create-db-instance",
)({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.query({
        ...rdsEngineVersionsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...rdsSubnetGroupsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...rdsParameterGroupsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2SecurityGroupsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2VpcsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2ImagesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Create Database | RDS | Mulga",
      },
    ],
  }),
  component: CreateDBInstancePage,
})
