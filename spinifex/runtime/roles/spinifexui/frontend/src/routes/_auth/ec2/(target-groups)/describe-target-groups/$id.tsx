import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { safeDecodeURIComponent } from "@/lib/utils"
import { ec2InstancesQueryOptions } from "@/queries/ec2"
import {
  elbv2TagsQueryOptions,
  elbv2TargetGroupAttributesQueryOptions,
  elbv2TargetGroupQueryOptions,
} from "@/queries/elbv2"

import { TargetGroupDetails } from "../-components/target-group-details"

export const Route = createFileRoute(
  "/_auth/ec2/(target-groups)/describe-target-groups/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    const arn = safeDecodeURIComponent(params.id)
    await Promise.all([
      context.queryClient.query({
        ...elbv2TargetGroupQueryOptions(arn),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2TargetGroupAttributesQueryOptions(arn),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2TagsQueryOptions([arn]),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2InstancesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${safeDecodeURIComponent(params.id)} | Target Group | Mulga`,
      },
    ],
  }),
  component: TargetGroupDetail,
})

// The target group's own page: the side panel's tabs, laid out for the page.
function TargetGroupDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const arn = safeDecodeURIComponent(id)
  const { data } = useSuspenseQuery(elbv2TargetGroupQueryOptions(arn))
  const tg = data.TargetGroups?.[0]

  if (!tg?.TargetGroupArn) {
    return (
      <>
        <BackLink to="/ec2/describe-target-groups">
          Back to target groups
        </BackLink>
        <p className="text-muted-foreground">Target group not found.</p>
      </>
    )
  }

  return (
    <TargetGroupDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-target-groups" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
      tg={tg}
    />
  )
}
