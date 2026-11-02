import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { safeDecodeURIComponent } from "@/lib/utils"
import {
  rdsParameterGroupQueryOptions,
  rdsParametersQueryOptions,
  rdsTagsQueryOptions,
} from "@/queries/rds"
import { isDefaultParameterGroupName } from "@/types/rds"

import { DBParameterGroupDetails } from "../-components/db-parameter-group-details"

export const Route = createFileRoute(
  "/_auth/rds/(parameter-groups)/describe-db-parameter-groups/$name",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    const name = safeDecodeURIComponent(params.name)

    // The tags query keys off the ARN, which only the describe knows, so it is
    // warmed after the group rather than alongside it. A default group has no
    // stored record for tags to live on, so it is skipped entirely.
    const [group] = await Promise.all([
      context.queryClient.query({
        ...rdsParameterGroupQueryOptions(name),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...rdsParametersQueryOptions(name),
        staleTime: "static",
      }),
    ])

    const arn = isDefaultParameterGroupName(name)
      ? ""
      : (group.DBParameterGroups?.[0]?.DBParameterGroupArn ?? "")

    if (arn !== "") {
      await context.queryClient.query({
        ...rdsTagsQueryOptions(arn),
        staleTime: "static",
      })
    }
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${safeDecodeURIComponent(params.name)} | RDS | Mulga`,
      },
    ],
  }),
  component: DBParameterGroupDetail,
})

// The group's own page: the side panel's tabs, with room for the editor.
function DBParameterGroupDetail() {
  const { name } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()

  const { data } = useSuspenseQuery(
    rdsParameterGroupQueryOptions(safeDecodeURIComponent(name)),
  )

  const group = data.DBParameterGroups?.[0]

  if (!group?.DBParameterGroupName) {
    return (
      <>
        <BackLink to="/rds/describe-db-parameter-groups">
          Back to parameter groups
        </BackLink>
        <p className="text-muted-foreground">DB parameter group not found.</p>
      </>
    )
  }

  return (
    <DBParameterGroupDetails
      group={group}
      layout="page"
      onClose={() => {
        void navigate({ to: "/rds/describe-db-parameter-groups" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
