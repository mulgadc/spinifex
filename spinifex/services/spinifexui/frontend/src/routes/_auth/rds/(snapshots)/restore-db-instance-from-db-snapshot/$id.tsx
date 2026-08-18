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
      context.queryClient.ensureQueryData(
        rdsDBSnapshotQueryOptions(decodeURIComponent(params.id)),
      ),
      context.queryClient.ensureQueryData(rdsEngineVersionsQueryOptions),
      context.queryClient.ensureQueryData(rdsSubnetGroupsQueryOptions),
      context.queryClient.ensureQueryData(rdsParameterGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2ImagesQueryOptions),
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
