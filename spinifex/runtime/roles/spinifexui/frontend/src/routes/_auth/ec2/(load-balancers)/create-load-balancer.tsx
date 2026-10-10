import { createFileRoute } from "@tanstack/react-router"

import { acmCertificatesQueryOptions } from "@/queries/acm"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import {
  elbv2SslPoliciesQueryOptions,
  elbv2TargetGroupsQueryOptions,
} from "@/queries/elbv2"

import { CreateLoadBalancerPage } from "./-components/create-load-balancer-page"

export const Route = createFileRoute(
  "/_auth/ec2/(load-balancers)/create-load-balancer",
)({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2VpcsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2SubnetsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2SecurityGroupsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2TargetGroupsQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...acmCertificatesQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2SslPoliciesQueryOptions,
        staleTime: "static",
      }),
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
