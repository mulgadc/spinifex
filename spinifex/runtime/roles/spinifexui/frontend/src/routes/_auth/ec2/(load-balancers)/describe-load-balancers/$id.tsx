import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { safeDecodeURIComponent } from "@/lib/utils"
import { ec2SubnetsQueryOptions } from "@/queries/ec2"
import {
  elbv2ListenersQueryOptions,
  elbv2LoadBalancerAttributesQueryOptions,
  elbv2LoadBalancerQueryOptions,
  elbv2TagsQueryOptions,
  elbv2TargetGroupsQueryOptions,
} from "@/queries/elbv2"

import { LoadBalancerDetails } from "../-components/load-balancer-details"

export const Route = createFileRoute(
  "/_auth/ec2/(load-balancers)/describe-load-balancers/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    const arn = safeDecodeURIComponent(params.id)
    await Promise.all([
      context.queryClient.query({
        ...elbv2LoadBalancerQueryOptions(arn),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2ListenersQueryOptions(arn),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2LoadBalancerAttributesQueryOptions(arn),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2TagsQueryOptions([arn]),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2TargetGroupsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2SubnetsQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${safeDecodeURIComponent(params.id)} | Load Balancer | Mulga`,
      },
    ],
  }),
  component: LoadBalancerDetail,
})

// The load balancer's own page: the side panel's tabs, laid out for the page.
function LoadBalancerDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const arn = safeDecodeURIComponent(id)
  const { data } = useSuspenseQuery(elbv2LoadBalancerQueryOptions(arn))
  const lb = data.LoadBalancers?.[0]

  if (!lb?.LoadBalancerArn) {
    return (
      <>
        <BackLink to="/ec2/describe-load-balancers">
          Back to load balancers
        </BackLink>
        <p className="text-muted-foreground">Load balancer not found.</p>
      </>
    )
  }

  return (
    <LoadBalancerDetails
      layout="page"
      lb={lb}
      onClose={() => {
        void navigate({ to: "/ec2/describe-load-balancers" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
