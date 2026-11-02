import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2InstanceQueryOptions } from "@/queries/ec2"

import { InstanceDetails } from "../-components/instance-details"
import { instanceName } from "../-components/instances-page"

export const Route = createFileRoute(
  "/_auth/ec2/(instances)/describe-instances/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) =>
    await context.queryClient.query({
      ...ec2InstanceQueryOptions(params.id),
      staleTime: "static",
    }),
  head: ({ loaderData }) => ({
    meta: [
      {
        title: `${loaderData?.Reservations?.[0]?.Instances?.[0]?.InstanceId ?? "Instance"} | EC2 | Mulga`,
      },
    ],
  }),
  component: InstanceDetail,
})

// The instance's own page: the side panel's tabs, laid out for the full width.
function InstanceDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2InstanceQueryOptions(id))
  const instance = data.Reservations?.[0]?.Instances?.[0]

  if (!instance?.InstanceId) {
    return (
      <>
        <BackLink to="/ec2/describe-instances">Back to instances</BackLink>
        <p className="text-muted-foreground">Instance not found.</p>
      </>
    )
  }

  return (
    <InstanceDetails
      instance={instance}
      layout="page"
      name={instanceName(instance)}
      onClose={() => {
        void navigate({ to: "/ec2/describe-instances" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
