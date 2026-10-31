import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { safeDecodeURIComponent } from "@/lib/utils"
import {
  rdsAutomatedBackupsQueryOptions,
  rdsDBInstanceQueryOptions,
  rdsEventsQueryOptions,
  rdsInstanceDBSnapshotsQueryOptions,
  rdsTagsQueryOptions,
} from "@/queries/rds"

import { DBInstanceDetails } from "../-components/db-instance-details"

export const Route = createFileRoute(
  "/_auth/rds/(db-instances)/describe-db-instances/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    const id = safeDecodeURIComponent(params.id)

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
        title: `${safeDecodeURIComponent(params.id)} | RDS | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

// The database's own page: the side panel's tabs, laid out for the full width.
function RouteComponent() {
  const id = safeDecodeURIComponent(Route.useParams().id)
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(rdsDBInstanceQueryOptions(id))
  const instance = data.DBInstances?.[0]

  if (!instance?.DBInstanceIdentifier) {
    return (
      <>
        <BackLink to="/rds/describe-db-instances">Back to databases</BackLink>
        <p className="text-muted-foreground">DB instance not found.</p>
      </>
    )
  }

  return (
    <DBInstanceDetails
      instance={instance}
      layout="page"
      onClose={() => {
        void navigate({ to: "/rds/describe-db-instances" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
