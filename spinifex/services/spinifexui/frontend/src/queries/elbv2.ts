import {
  DescribeListenersCommand,
  DescribeLoadBalancerAttributesCommand,
  DescribeLoadBalancersCommand,
  DescribeRulesCommand,
  DescribeTagsCommand,
  DescribeTargetGroupAttributesCommand,
  DescribeTargetGroupsCommand,
  DescribeTargetHealthCommand,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { queryOptions } from "@tanstack/react-query"

import { getElbv2Client } from "@/lib/awsClient"

const PROVISIONING_POLL_MS = 5000

export const elbv2LoadBalancersQueryOptions = queryOptions({
  queryKey: ["elbv2", "loadBalancers"],
  queryFn: async () => {
    const command = new DescribeLoadBalancersCommand({})
    return await getElbv2Client().send(command)
  },
  refetchInterval: (query) => {
    const lbs = query.state.data?.LoadBalancers ?? []
    const anyProvisioning = lbs.some((lb) => lb.State?.Code === "provisioning")
    return anyProvisioning ? PROVISIONING_POLL_MS : false
  },
})

export const elbv2LoadBalancerQueryOptions = (arn: string) =>
  queryOptions({
    queryKey: ["elbv2", "loadBalancers", arn],
    queryFn: async () => {
      const command = new DescribeLoadBalancersCommand({
        LoadBalancerArns: [arn],
      })
      return await getElbv2Client().send(command)
    },
    refetchInterval: (query) => {
      const lb = query.state.data?.LoadBalancers?.[0]
      return lb?.State?.Code === "provisioning" ? PROVISIONING_POLL_MS : false
    },
  })

export const elbv2LoadBalancerAttributesQueryOptions = (arn: string) =>
  queryOptions({
    queryKey: ["elbv2", "loadBalancers", arn, "attributes"],
    queryFn: async () => {
      const command = new DescribeLoadBalancerAttributesCommand({
        LoadBalancerArn: arn,
      })
      return await getElbv2Client().send(command)
    },
  })

export const elbv2TargetGroupsQueryOptions = queryOptions({
  queryKey: ["elbv2", "targetGroups"],
  queryFn: async () => {
    const command = new DescribeTargetGroupsCommand({})
    return await getElbv2Client().send(command)
  },
})

export const elbv2TargetGroupQueryOptions = (arn: string) =>
  queryOptions({
    queryKey: ["elbv2", "targetGroups", arn],
    queryFn: async () => {
      const command = new DescribeTargetGroupsCommand({
        TargetGroupArns: [arn],
      })
      return await getElbv2Client().send(command)
    },
  })

export const elbv2TargetGroupAttributesQueryOptions = (arn: string) =>
  queryOptions({
    queryKey: ["elbv2", "targetGroups", arn, "attributes"],
    queryFn: async () => {
      const command = new DescribeTargetGroupAttributesCommand({
        TargetGroupArn: arn,
      })
      return await getElbv2Client().send(command)
    },
  })

export const elbv2ListenersQueryOptions = (loadBalancerArn: string) =>
  queryOptions({
    queryKey: ["elbv2", "listeners", loadBalancerArn],
    queryFn: async () => {
      const command = new DescribeListenersCommand({
        LoadBalancerArn: loadBalancerArn,
      })
      return await getElbv2Client().send(command)
    },
  })

export const elbv2ListenerRulesQueryOptions = (listenerArn: string) =>
  queryOptions({
    queryKey: ["elbv2", "listeners", listenerArn, "rules"],
    queryFn: async () => {
      const command = new DescribeRulesCommand({ ListenerArn: listenerArn })
      return await getElbv2Client().send(command)
    },
  })

// Polling is opt-in by the caller (slice 5 enables 5s refetch while the Targets
// tab is visible). List page uses this as a one-shot for the health summary.
export const elbv2TargetHealthQueryOptions = (targetGroupArn: string) =>
  queryOptions({
    queryKey: ["elbv2", "targetGroups", targetGroupArn, "health"],
    queryFn: async () => {
      const command = new DescribeTargetHealthCommand({
        TargetGroupArn: targetGroupArn,
      })
      return await getElbv2Client().send(command)
    },
  })

export const elbv2TagsQueryOptions = (resourceArns: string[]) =>
  queryOptions({
    queryKey: ["elbv2", "tags", ...resourceArns],
    queryFn: async () => {
      const command = new DescribeTagsCommand({
        ResourceArns: resourceArns,
      })
      return await getElbv2Client().send(command)
    },
    enabled: resourceArns.length > 0,
  })
