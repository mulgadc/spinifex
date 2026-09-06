import { createFileRoute } from "@tanstack/react-router"

import {
  rdsAutomatedBackupsQueryOptions,
  rdsDBInstanceQueryOptions,
  rdsEventsQueryOptions,
  rdsInstanceDBSnapshotsQueryOptions,
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
      context.queryClient.query({
        ...rdsDBInstanceQueryOptions(id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...rdsEventsQueryOptions(id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...rdsInstanceDBSnapshotsQueryOptions(id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...rdsAutomatedBackupsQueryOptions(id),
        staleTime: "static",
      }),
    ])
    const arn = instance.DBInstances?.[0]?.DBInstanceArn ?? ""
    await context.queryClient.query({
      ...rdsTagsQueryOptions(arn),
      staleTime: "static",
    })
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
