import {
  DescribeClustersCommand,
  DescribeContainerInstancesCommand,
  DescribeServicesCommand,
  DescribeTaskDefinitionCommand,
  DescribeTasksCommand,
  ListClustersCommand,
  ListContainerInstancesCommand,
  ListServicesCommand,
  ListTaskDefinitionsCommand,
  ListTasksCommand,
} from "@aws-sdk/client-ecs"
import { queryOptions } from "@tanstack/react-query"

import { getEcsClient } from "@/lib/awsClient"

const CLUSTER_STALE_TIME = 30_000
const TASK_STALE_TIME = 15_000

// ecsClustersQueryOptions lists cluster ARNs then describes them so the list can
// render names and status in one query. ListClusters returns ARNs only.
export const ecsClustersQueryOptions = queryOptions({
  queryKey: ["ecs", "clusters"],
  queryFn: async () => {
    const client = getEcsClient()
    const list = await client.send(new ListClustersCommand({}))
    const clusterArns = list.clusterArns ?? []
    if (clusterArns.length === 0) {
      return []
    }
    const described = await client.send(
      new DescribeClustersCommand({ clusters: clusterArns }),
    )
    return described.clusters ?? []
  },
  staleTime: CLUSTER_STALE_TIME,
})

export const ecsClusterQueryOptions = (clusterName: string) =>
  queryOptions({
    queryKey: ["ecs", "clusters", clusterName],
    queryFn: async () => {
      const described = await getEcsClient().send(
        new DescribeClustersCommand({ clusters: [clusterName] }),
      )
      return described.clusters?.[0]
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const ecsServicesQueryOptions = (clusterName: string) =>
  queryOptions({
    queryKey: ["ecs", "clusters", clusterName, "services"],
    queryFn: async () => {
      const client = getEcsClient()
      const list = await client.send(
        new ListServicesCommand({ cluster: clusterName }),
      )
      const serviceArns = list.serviceArns ?? []
      if (serviceArns.length === 0) {
        return []
      }
      const described = await client.send(
        new DescribeServicesCommand({
          cluster: clusterName,
          services: serviceArns,
        }),
      )
      return described.services ?? []
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const ecsTasksQueryOptions = (clusterName: string) =>
  queryOptions({
    queryKey: ["ecs", "clusters", clusterName, "tasks"],
    queryFn: async () => {
      const client = getEcsClient()
      const list = await client.send(
        new ListTasksCommand({ cluster: clusterName }),
      )
      const taskArns = list.taskArns ?? []
      if (taskArns.length === 0) {
        return []
      }
      const described = await client.send(
        new DescribeTasksCommand({ cluster: clusterName, tasks: taskArns }),
      )
      return described.tasks ?? []
    },
    staleTime: TASK_STALE_TIME,
  })

export const ecsContainerInstancesQueryOptions = (clusterName: string) =>
  queryOptions({
    queryKey: ["ecs", "clusters", clusterName, "container-instances"],
    queryFn: async () => {
      const client = getEcsClient()
      const list = await client.send(
        new ListContainerInstancesCommand({ cluster: clusterName }),
      )
      const containerInstanceArns = list.containerInstanceArns ?? []
      if (containerInstanceArns.length === 0) {
        return []
      }
      const described = await client.send(
        new DescribeContainerInstancesCommand({
          cluster: clusterName,
          containerInstances: containerInstanceArns,
        }),
      )
      return described.containerInstances ?? []
    },
    staleTime: CLUSTER_STALE_TIME,
  })

// ecsTaskDefinitionsQueryOptions returns the family:revision ARNs. Task
// definitions are account-scoped, so this query is not cluster-keyed.
export const ecsTaskDefinitionsQueryOptions = queryOptions({
  queryKey: ["ecs", "task-definitions"],
  queryFn: async () => {
    const list = await getEcsClient().send(new ListTaskDefinitionsCommand({}))
    return list.taskDefinitionArns ?? []
  },
  staleTime: CLUSTER_STALE_TIME,
})

// ecsTaskDefinitionQueryOptions resolves a single revision so a wizard can read
// its network mode (awsvpc gating) and default container name/port for a load
// balancer. The ref is "family", "family:revision", or an ARN.
export const ecsTaskDefinitionQueryOptions = (taskDefinition: string) =>
  queryOptions({
    queryKey: ["ecs", "task-definitions", taskDefinition],
    queryFn: async () => {
      const out = await getEcsClient().send(
        new DescribeTaskDefinitionCommand({ taskDefinition }),
      )
      return out.taskDefinition
    },
    enabled: taskDefinition !== "",
    staleTime: CLUSTER_STALE_TIME,
  })
