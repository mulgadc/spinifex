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
      context.queryClient.ensureQueryData(rdsEngineVersionsQueryOptions),
      context.queryClient.ensureQueryData(rdsSubnetGroupsQueryOptions),
      context.queryClient.ensureQueryData(rdsParameterGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2VpcsQueryOptions),
      context.queryClient.ensureQueryData(ec2ImagesQueryOptions),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Create database | RDS | Mulga",
      },
    ],
  }),
  component: CreateDBInstancePage,
})
