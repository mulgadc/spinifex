import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2SecurityGroupQueryOptions } from "@/queries/ec2"

import { SecurityGroupDetails } from "../-components/security-group-details"

export const Route = createFileRoute(
  "/_auth/ec2/(security-groups)/describe-security-groups/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2SecurityGroupQueryOptions(params.id),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Security Group | Mulga`,
      },
    ],
  }),
  component: SecurityGroupDetail,
})

// The group's own page: the side panel's tabs, laid out for the full width.
function SecurityGroupDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2SecurityGroupQueryOptions(id))
  const sg = data.SecurityGroups?.[0]

  if (!sg?.GroupId) {
    return (
      <>
        <BackLink to="/ec2/describe-security-groups">
          Back to security groups
        </BackLink>
        <p className="text-muted-foreground">Security group not found.</p>
      </>
    )
  }

  return (
    <SecurityGroupDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-security-groups" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      sg={sg}
      tab={tab}
    />
  )
}
