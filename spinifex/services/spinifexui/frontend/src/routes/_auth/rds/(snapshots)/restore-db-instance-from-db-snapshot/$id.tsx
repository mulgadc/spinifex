import { createFileRoute } from "@tanstack/react-router"

import {
  ec2ImagesQueryOptions,
  ec2SecurityGroupsQueryOptions,
} from "@/queries/ec2"
import {
  rdsDBSnapshotQueryOptions,
  rdsEngineVersionsQueryOptions,
  rdsParameterGroupsQueryOptions,
  rdsSubnetGroupsQueryOptions,
} from "@/queries/rds"

import { RestoreDBSnapshotPage } from "../-components/restore-db-snapshot-page"

export const Route = createFileRoute(
  "/_auth/rds/(snapshots)/restore-db-instance-from-db-snapshot/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...rdsDBSnapshotQueryOptions(decodeURIComponent(params.id)),
        staleTime: "static",
      }),
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
        ...ec2ImagesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `Restore ${decodeURIComponent(params.id)} | RDS | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { id } = Route.useParams()
  return <RestoreDBSnapshotPage dbSnapshotIdentifier={decodeURIComponent(id)} />
}
