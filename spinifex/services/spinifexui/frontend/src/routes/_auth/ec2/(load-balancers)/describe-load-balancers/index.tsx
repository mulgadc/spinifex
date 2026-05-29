import { createFileRoute } from "@tanstack/react-router"

import { elbv2LoadBalancersQueryOptions } from "@/queries/elbv2"

import { DescribeLoadBalancersPage } from "../-components/describe-load-balancers-page"

export const Route = createFileRoute(
  "/_auth/ec2/(load-balancers)/describe-load-balancers/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(elbv2LoadBalancersQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Load Balancers | EC2 | Mulga",
      },
    ],
  }),
  component: DescribeLoadBalancersPage,
})
