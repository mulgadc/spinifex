import { createFileRoute } from "@tanstack/react-router"

import { rdsSubnetGroupQueryOptions, rdsTagsQueryOptions } from "@/queries/rds"

import { DBSubnetGroupDetailPage } from "../-components/db-subnet-group-detail-page"

export const Route = createFileRoute(
  "/_auth/rds/(subnet-groups)/describe-db-subnet-groups/$name",
)({
  loader: async ({ context, params }) => {
    const name = decodeURIComponent(params.name)
    // The tags query keys off the ARN, which only the describe knows, so it is
    // warmed after the group rather than alongside it.
    const group = await context.queryClient.ensureQueryData(
      rdsSubnetGroupQueryOptions(name),
    )
    const arn = group.DBSubnetGroups?.[0]?.DBSubnetGroupArn ?? ""
    await context.queryClient.ensureQueryData(rdsTagsQueryOptions(arn))
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${decodeURIComponent(params.name)} | RDS | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { name } = Route.useParams()
  return (
    <DBSubnetGroupDetailPage dbSubnetGroupName={decodeURIComponent(name)} />
  )
}
