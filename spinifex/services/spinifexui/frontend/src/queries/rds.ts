import {
  DescribeDBEngineVersionsCommand,
  DescribeDBInstancesCommand,
  DescribeDBParameterGroupsCommand,
  DescribeDBParametersCommand,
  DescribeDBSubnetGroupsCommand,
  DescribeEventsCommand,
  DescribeOrderableDBInstanceOptionsCommand,
  ListTagsForResourceCommand,
} from "@aws-sdk/client-rds"
import { queryOptions } from "@tanstack/react-query"

import { getRdsClient } from "@/lib/awsClient"
import { isTransitionalStatus } from "@/types/rds"

// Creates take minutes and half the RDS statuses are transitional, so a list
// that never refetches reads as broken. Polling stops once everything settles.
const TRANSITIONAL_POLL_MS = 5000

// The event ring holds 14 days; DescribeEvents defaults to the last hour, so
// the whole ring has to be asked for explicitly.
const EVENT_WINDOW_MINUTES = 14 * 24 * 60

// The engine and class catalogs change at most once a release, and both are
// read on every create form render.
const CATALOG_STALE_TIME_MS = 60 * 60 * 1000

const DB_INSTANCE_SOURCE_TYPE = "db-instance"

export const rdsDBInstancesQueryOptions = queryOptions({
  queryKey: ["rds", "dbInstances"],
  queryFn: async () => {
    const command = new DescribeDBInstancesCommand({})
    return await getRdsClient().send(command)
  },
  refetchInterval: (query) => {
    const instances = query.state.data?.DBInstances ?? []
    const anyTransitional = instances.some((instance) =>
      isTransitionalStatus(instance.DBInstanceStatus),
    )
    return anyTransitional ? TRANSITIONAL_POLL_MS : false
  },
})

export const rdsDBInstanceQueryOptions = (dbInstanceIdentifier: string) =>
  queryOptions({
    queryKey: ["rds", "dbInstances", dbInstanceIdentifier],
    queryFn: async () => {
      const command = new DescribeDBInstancesCommand({
        DBInstanceIdentifier: dbInstanceIdentifier,
      })
      return await getRdsClient().send(command)
    },
    refetchInterval: (query) => {
      const instance = query.state.data?.DBInstances?.[0]
      return isTransitionalStatus(instance?.DBInstanceStatus)
        ? TRANSITIONAL_POLL_MS
        : false
    },
  })

export const rdsSubnetGroupsQueryOptions = queryOptions({
  queryKey: ["rds", "subnetGroups"],
  queryFn: async () => {
    const command = new DescribeDBSubnetGroupsCommand({})
    return await getRdsClient().send(command)
  },
})

export const rdsSubnetGroupQueryOptions = (dbSubnetGroupName: string) =>
  queryOptions({
    queryKey: ["rds", "subnetGroups", dbSubnetGroupName],
    queryFn: async () => {
      const command = new DescribeDBSubnetGroupsCommand({
        DBSubnetGroupName: dbSubnetGroupName,
      })
      return await getRdsClient().send(command)
    },
  })

export const rdsParameterGroupsQueryOptions = queryOptions({
  queryKey: ["rds", "parameterGroups"],
  queryFn: async () => {
    const command = new DescribeDBParameterGroupsCommand({})
    return await getRdsClient().send(command)
  },
})

export const rdsParameterGroupQueryOptions = (dbParameterGroupName: string) =>
  queryOptions({
    queryKey: ["rds", "parameterGroups", dbParameterGroupName],
    queryFn: async () => {
      const command = new DescribeDBParameterGroupsCommand({
        DBParameterGroupName: dbParameterGroupName,
      })
      return await getRdsClient().send(command)
    },
  })

// Unfiltered: the whole set is one round trip, and every row already carries
// the Source field the user-versus-engine-default filter reads.
export const rdsParametersQueryOptions = (dbParameterGroupName: string) =>
  queryOptions({
    queryKey: ["rds", "parameters", dbParameterGroupName],
    queryFn: async () => {
      const command = new DescribeDBParametersCommand({
        DBParameterGroupName: dbParameterGroupName,
      })
      return await getRdsClient().send(command)
    },
  })

export const rdsEventsQueryOptions = (sourceIdentifier: string) =>
  queryOptions({
    queryKey: ["rds", "events", sourceIdentifier],
    queryFn: async () => {
      const command = new DescribeEventsCommand({
        SourceIdentifier: sourceIdentifier,
        SourceType: DB_INSTANCE_SOURCE_TYPE,
        Duration: EVENT_WINDOW_MINUTES,
      })
      return await getRdsClient().send(command)
    },
  })

export const rdsTagsQueryOptions = (resourceName: string) =>
  queryOptions({
    queryKey: ["rds", "tags", resourceName],
    queryFn: async () => {
      const command = new ListTagsForResourceCommand({
        ResourceName: resourceName,
      })
      return await getRdsClient().send(command)
    },
    enabled: resourceName.length > 0,
  })

// The source for the engine and version pickers. The console never restates
// which engines or versions exist; this is the only answer.
export const rdsEngineVersionsQueryOptions = queryOptions({
  queryKey: ["rds", "engineVersions"],
  queryFn: async () => {
    const command = new DescribeDBEngineVersionsCommand({})
    return await getRdsClient().send(command)
  },
  staleTime: CATALOG_STALE_TIME_MS,
})

// Filtered by what this cluster's nodes can actually run, so it can legitimately
// come back short or empty. An empty list is an explanation to render, not a
// loading state.
export const rdsOrderableOptionsQueryOptions = (engine: string) =>
  queryOptions({
    queryKey: ["rds", "orderableOptions", engine],
    queryFn: async () => {
      const command = new DescribeOrderableDBInstanceOptionsCommand({
        Engine: engine,
      })
      return await getRdsClient().send(command)
    },
    staleTime: CATALOG_STALE_TIME_MS,
    enabled: engine.length > 0,
  })
