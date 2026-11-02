import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { safeDecodeURIComponent } from "@/lib/utils"
import { rdsSubnetGroupQueryOptions, rdsTagsQueryOptions } from "@/queries/rds"

import { DBSubnetGroupDetails } from "../-components/db-subnet-group-details"

export const Route = createFileRoute(
  "/_auth/rds/(subnet-groups)/describe-db-subnet-groups/$name",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    const name = safeDecodeURIComponent(params.name)

    // The tags query keys off the ARN, which only the describe knows, so it is
    // warmed after the group rather than alongside it.
    const group = await context.queryClient.query({
      ...rdsSubnetGroupQueryOptions(name),
      staleTime: "static",
    })

    const arn = group.DBSubnetGroups?.[0]?.DBSubnetGroupArn ?? ""

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
  component: DBSubnetGroupDetail,
})

// The group's own page: the side panel's tabs, laid out for the full width.
function DBSubnetGroupDetail() {
  const { name } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()

  const { data } = useSuspenseQuery(
    rdsSubnetGroupQueryOptions(safeDecodeURIComponent(name)),
  )

  const group = data.DBSubnetGroups?.[0]

  if (!group?.DBSubnetGroupName) {
    return (
      <>
        <BackLink to="/rds/describe-db-subnet-groups">
          Back to subnet groups
        </BackLink>
        <p className="text-muted-foreground">DB subnet group not found.</p>
      </>
    )
  }

  return (
    <DBSubnetGroupDetails
      group={group}
      layout="page"
      onClose={() => {
        void navigate({ to: "/rds/describe-db-subnet-groups" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
