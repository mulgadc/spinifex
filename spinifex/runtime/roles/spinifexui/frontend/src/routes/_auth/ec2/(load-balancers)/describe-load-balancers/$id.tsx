import { createFileRoute } from "@tanstack/react-router"

import { safeDecodeURIComponent } from "@/lib/utils"
import { ec2SubnetsQueryOptions } from "@/queries/ec2"
import {
  elbv2ListenersQueryOptions,
  elbv2LoadBalancerAttributesQueryOptions,
  elbv2LoadBalancerQueryOptions,
  elbv2TagsQueryOptions,
  elbv2TargetGroupsQueryOptions,
} from "@/queries/elbv2"

import { LoadBalancerDetailPage } from "../-components/load-balancer-detail-page"

export const Route = createFileRoute(
  "/_auth/ec2/(load-balancers)/describe-load-balancers/$id",
)({
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
  component: RouteComponent,
})

function RouteComponent() {
  const { id } = Route.useParams()
  return <LoadBalancerDetailPage arn={safeDecodeURIComponent(id)} />
}
