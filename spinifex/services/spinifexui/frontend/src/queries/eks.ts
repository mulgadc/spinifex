import {
  DescribeAccessEntryCommand,
  DescribeAddonCommand,
  DescribeAddonVersionsCommand,
  DescribeClusterCommand,
  DescribeNodegroupCommand,
  ListAccessEntriesCommand,
  ListAccessPoliciesCommand,
  ListAddonsCommand,
  ListAssociatedAccessPoliciesCommand,
  ListClustersCommand,
  ListNodegroupsCommand,
} from "@aws-sdk/client-eks"
import { queryOptions } from "@tanstack/react-query"

import { getEksClient } from "@/lib/awsClient"

const CLUSTER_STALE_TIME = 30_000
const STATIC_STALE_TIME = 300_000

export const eksClustersQueryOptions = queryOptions({
  queryKey: ["eks", "clusters"],
  queryFn: async () => {
    const command = new ListClustersCommand({})
    return await getEksClient().send(command)
  },
  staleTime: CLUSTER_STALE_TIME,
})

export const eksClusterQueryOptions = (clusterName: string) =>
  queryOptions({
    queryKey: ["eks", "clusters", clusterName],
    queryFn: async () => {
      const command = new DescribeClusterCommand({ name: clusterName })
      return await getEksClient().send(command)
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const eksNodegroupsQueryOptions = (clusterName: string) =>
  queryOptions({
    queryKey: ["eks", "clusters", clusterName, "nodegroups"],
    queryFn: async () => {
      const command = new ListNodegroupsCommand({ clusterName })
      return await getEksClient().send(command)
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const eksNodegroupQueryOptions = (
  clusterName: string,
  nodegroupName: string,
) =>
  queryOptions({
    queryKey: ["eks", "clusters", clusterName, "nodegroups", nodegroupName],
    queryFn: async () => {
      const command = new DescribeNodegroupCommand({
        clusterName,
        nodegroupName,
      })
      return await getEksClient().send(command)
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const eksAccessEntriesQueryOptions = (clusterName: string) =>
  queryOptions({
    queryKey: ["eks", "clusters", clusterName, "access-entries"],
    queryFn: async () => {
      const command = new ListAccessEntriesCommand({ clusterName })
      return await getEksClient().send(command)
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const eksAccessEntryQueryOptions = (
  clusterName: string,
  principalArn: string,
) =>
  queryOptions({
    queryKey: ["eks", "clusters", clusterName, "access-entries", principalArn],
    queryFn: async () => {
      const command = new DescribeAccessEntryCommand({
        clusterName,
        principalArn,
      })
      return await getEksClient().send(command)
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const eksAssociatedAccessPoliciesQueryOptions = (
  clusterName: string,
  principalArn: string,
) =>
  queryOptions({
    queryKey: [
      "eks",
      "clusters",
      clusterName,
      "access-entries",
      principalArn,
      "policies",
    ],
    queryFn: async () => {
      const command = new ListAssociatedAccessPoliciesCommand({
        clusterName,
        principalArn,
      })
      return await getEksClient().send(command)
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const eksAccessPoliciesQueryOptions = queryOptions({
  queryKey: ["eks", "access-policies"],
  queryFn: async () => {
    const command = new ListAccessPoliciesCommand({})
    return await getEksClient().send(command)
  },
  staleTime: STATIC_STALE_TIME,
})

export const eksAddonVersionsQueryOptions = queryOptions({
  queryKey: ["eks", "addon-versions"],
  queryFn: async () => {
    const command = new DescribeAddonVersionsCommand({})
    return await getEksClient().send(command)
  },
  staleTime: STATIC_STALE_TIME,
})

export const eksAddonsQueryOptions = (clusterName: string) =>
  queryOptions({
    queryKey: ["eks", "clusters", clusterName, "addons"],
    queryFn: async () => {
      const command = new ListAddonsCommand({ clusterName })
      return await getEksClient().send(command)
    },
    staleTime: CLUSTER_STALE_TIME,
  })

export const eksAddonQueryOptions = (clusterName: string, addonName: string) =>
  queryOptions({
    queryKey: ["eks", "clusters", clusterName, "addons", addonName],
    queryFn: async () => {
      const command = new DescribeAddonCommand({ clusterName, addonName })
      return await getEksClient().send(command)
    },
    staleTime: CLUSTER_STALE_TIME,
  })
