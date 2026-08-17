import { createFileRoute } from "@tanstack/react-router"

import { ec2SecurityGroupsQueryOptions } from "@/queries/ec2"
import {
  rdsAutomatedBackupsQueryOptions,
  rdsDBInstanceQueryOptions,
  rdsEventsQueryOptions,
  rdsInstanceDBSnapshotsQueryOptions,
  rdsParameterGroupsQueryOptions,
  rdsTagsQueryOptions,
} from "@/queries/rds"

import { DBInstanceDetailPage } from "../-components/db-instance-detail-page"

export const Route = createFileRoute(
  "/_auth/rds/(db-instances)/describe-db-instances/$id",
)({
  loader: async ({ context, params }) => {
    const id = decodeURIComponent(params.id)
    // The tags query keys off the ARN, which only the describe knows, so it is
    // warmed after the instance rather than alongside it.
    const [instance] = await Promise.all([
      context.queryClient.ensureQueryData(rdsDBInstanceQueryOptions(id)),
      context.queryClient.ensureQueryData(rdsEventsQueryOptions(id)),
      context.queryClient.ensureQueryData(
        rdsInstanceDBSnapshotsQueryOptions(id),
      ),
      context.queryClient.ensureQueryData(rdsAutomatedBackupsQueryOptions(id)),
      context.queryClient.ensureQueryData(rdsParameterGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
    ])
    const arn = instance.DBInstances?.[0]?.DBInstanceArn ?? ""
    await context.queryClient.ensureQueryData(rdsTagsQueryOptions(arn))
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${decodeURIComponent(params.id)} | RDS | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { id } = Route.useParams()
  return <DBInstanceDetailPage dbInstanceIdentifier={decodeURIComponent(id)} />
}
