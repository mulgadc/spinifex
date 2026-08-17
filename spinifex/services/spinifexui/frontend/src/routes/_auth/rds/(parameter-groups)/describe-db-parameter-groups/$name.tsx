import { createFileRoute } from "@tanstack/react-router"

import {
  rdsParameterGroupQueryOptions,
  rdsParametersQueryOptions,
  rdsTagsQueryOptions,
} from "@/queries/rds"
import { isDefaultParameterGroupName } from "@/types/rds"

import { DBParameterGroupDetailPage } from "../-components/db-parameter-group-detail-page"

export const Route = createFileRoute(
  "/_auth/rds/(parameter-groups)/describe-db-parameter-groups/$name",
)({
  loader: async ({ context, params }) => {
    const name = decodeURIComponent(params.name)
    // The tags query keys off the ARN, which only the describe knows, so it is
    // warmed after the group rather than alongside it. A default group has no
    // stored record for tags to live on, so it is skipped entirely.
    const [group] = await Promise.all([
      context.queryClient.ensureQueryData(rdsParameterGroupQueryOptions(name)),
      context.queryClient.ensureQueryData(rdsParametersQueryOptions(name)),
    ])
    const arn = isDefaultParameterGroupName(name)
      ? ""
      : (group.DBParameterGroups?.[0]?.DBParameterGroupArn ?? "")
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
    <DBParameterGroupDetailPage
      dbParameterGroupName={decodeURIComponent(name)}
    />
  )
}
