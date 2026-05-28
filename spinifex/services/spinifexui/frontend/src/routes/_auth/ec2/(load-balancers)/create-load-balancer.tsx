import { createFileRoute } from "@tanstack/react-router"

import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { elbv2TargetGroupsQueryOptions } from "@/queries/elbv2"

import { CreateLoadBalancerPage } from "./-components/create-load-balancer-page"

export const Route = createFileRoute(
  "/_auth/ec2/(load-balancers)/create-load-balancer",
)({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2VpcsQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
      context.queryClient.ensureQueryData(elbv2TargetGroupsQueryOptions),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Create Load Balancer | EC2 | Mulga",
      },
    ],
  }),
  component: CreateLoadBalancerPage,
})
