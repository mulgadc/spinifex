import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

import {
  rdsDBInstanceQueryOptions,
  rdsDBInstancesQueryOptions,
  rdsEngineVersionsQueryOptions,
  rdsEventsQueryOptions,
  rdsOrderableOptionsQueryOptions,
  rdsParameterGroupQueryOptions,
  rdsParameterGroupsQueryOptions,
  rdsParametersQueryOptions,
  rdsSubnetGroupQueryOptions,
  rdsSubnetGroupsQueryOptions,
  rdsTagsQueryOptions,
} from "./rds"

describe("rds query keys", () => {
  it("rdsDBInstancesQueryOptions has correct key", () => {
    expect(rdsDBInstancesQueryOptions.queryKey).toStrictEqual([
      "rds",
      "dbInstances",
    ])
  })

  it("rdsDBInstanceQueryOptions includes the identifier", () => {
    expect(rdsDBInstanceQueryOptions("orders-db").queryKey).toStrictEqual([
      "rds",
      "dbInstances",
      "orders-db",
    ])
  })

  it("rdsSubnetGroupsQueryOptions has correct key", () => {
    expect(rdsSubnetGroupsQueryOptions.queryKey).toStrictEqual([
      "rds",
      "subnetGroups",
    ])
  })

  it("rdsParameterGroupsQueryOptions has correct key", () => {
    expect(rdsParameterGroupsQueryOptions.queryKey).toStrictEqual([
      "rds",
      "parameterGroups",
    ])
  })

  it("rdsSubnetGroupQueryOptions includes the group name", () => {
    expect(rdsSubnetGroupQueryOptions("orders-subnets").queryKey).toStrictEqual(
      ["rds", "subnetGroups", "orders-subnets"],
    )
  })

  it("rdsParameterGroupQueryOptions includes the group name", () => {
    expect(rdsParameterGroupQueryOptions("orders-pg").queryKey).toStrictEqual([
      "rds",
      "parameterGroups",
      "orders-pg",
    ])
  })

  it("rdsParametersQueryOptions includes the group name", () => {
    expect(rdsParametersQueryOptions("orders-pg").queryKey).toStrictEqual([
      "rds",
      "parameters",
      "orders-pg",
    ])
  })

  it("rdsEventsQueryOptions includes the source identifier", () => {
    expect(rdsEventsQueryOptions("orders-db").queryKey).toStrictEqual([
      "rds",
      "events",
      "orders-db",
    ])
  })

  it("rdsTagsQueryOptions includes the resource name", () => {
    expect(rdsTagsQueryOptions("arn:db").queryKey).toStrictEqual([
      "rds",
      "tags",
      "arn:db",
    ])
  })

  it("rdsEngineVersionsQueryOptions has correct key", () => {
    expect(rdsEngineVersionsQueryOptions.queryKey).toStrictEqual([
      "rds",
      "engineVersions",
    ])
  })

  it("rdsOrderableOptionsQueryOptions includes the engine", () => {
    expect(rdsOrderableOptionsQueryOptions("postgres").queryKey).toStrictEqual([
      "rds",
      "orderableOptions",
      "postgres",
    ])
  })
})

type QueryFnWithSignal = (ctx: { signal: AbortSignal }) => Promise<unknown>

async function callQueryFn(queryFn: unknown): Promise<unknown> {
  return await (queryFn as QueryFnWithSignal)({
    signal: new AbortController().signal,
  })
}

describe("rds queries send the right command", () => {
  it("dbInstances list sends an unfiltered describe", async () => {
    mockSend.mockResolvedValueOnce({ DBInstances: [] })
    await callQueryFn(rdsDBInstancesQueryOptions.queryFn)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("dbInstance detail filters by identifier", async () => {
    mockSend.mockResolvedValueOnce({ DBInstances: [] })
    await callQueryFn(rdsDBInstanceQueryOptions("orders-db").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db",
    })
  })

  it("events asks for the whole 14-day ring, not the default hour", async () => {
    mockSend.mockResolvedValueOnce({ Events: [] })
    await callQueryFn(rdsEventsQueryOptions("orders-db").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SourceIdentifier: "orders-db",
      SourceType: "db-instance",
      Duration: 14 * 24 * 60,
    })
  })

  it("tags sends the resource name", async () => {
    mockSend.mockResolvedValueOnce({ TagList: [] })
    await callQueryFn(rdsTagsQueryOptions("arn:db").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      ResourceName: "arn:db",
    })
  })

  it("orderable options filter by engine", async () => {
    mockSend.mockResolvedValueOnce({ OrderableDBInstanceOptions: [] })
    await callQueryFn(rdsOrderableOptionsQueryOptions("mariadb").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Engine: "mariadb",
    })
  })

  it("subnet groups and parameter groups send unfiltered describes", async () => {
    mockSend.mockResolvedValueOnce({ DBSubnetGroups: [] })
    await callQueryFn(rdsSubnetGroupsQueryOptions.queryFn)
    mockSend.mockResolvedValueOnce({ DBParameterGroups: [] })
    await callQueryFn(rdsParameterGroupsQueryOptions.queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
    expect(mockSend.mock.calls[1]?.[0].input).toStrictEqual({})
  })

  it("the single group describes filter by name", async () => {
    mockSend.mockResolvedValueOnce({ DBSubnetGroups: [] })
    await callQueryFn(rdsSubnetGroupQueryOptions("orders-subnets").queryFn)
    mockSend.mockResolvedValueOnce({ DBParameterGroups: [] })
    await callQueryFn(rdsParameterGroupQueryOptions("orders-pg").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBSubnetGroupName: "orders-subnets",
    })
    expect(mockSend.mock.calls[1]?.[0].input).toStrictEqual({
      DBParameterGroupName: "orders-pg",
    })
  })

  // The Source filter the backend accepts is deliberately unused: the whole
  // set is one round trip and the split is a client-side filter.
  it("parameters are fetched unfiltered by source", async () => {
    mockSend.mockResolvedValueOnce({ Parameters: [] })
    await callQueryFn(rdsParametersQueryOptions("orders-pg").queryFn)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBParameterGroupName: "orders-pg",
    })
  })
})

function refetchIntervalOf(options: {
  refetchInterval?: unknown
}): (query: unknown) => unknown {
  const refetch = options.refetchInterval
  if (typeof refetch !== "function") {
    throw new TypeError("expected refetchInterval to be a function")
  }
  return refetch as (query: unknown) => unknown
}

describe("rds poll cadence", () => {
  it("polls the list while any instance is creating", () => {
    const refetch = refetchIntervalOf(rdsDBInstancesQueryOptions)
    expect(
      refetch({
        state: { data: { DBInstances: [{ DBInstanceStatus: "creating" }] } },
      }),
    ).toBe(5000)
  })

  it("stops polling the list once every instance is available", () => {
    const refetch = refetchIntervalOf(rdsDBInstancesQueryOptions)
    expect(
      refetch({
        state: { data: { DBInstances: [{ DBInstanceStatus: "available" }] } },
      }),
    ).toBeFalsy()
  })

  it("does not treat a stopped instance as in flight", () => {
    const refetch = refetchIntervalOf(rdsDBInstancesQueryOptions)
    expect(
      refetch({
        state: { data: { DBInstances: [{ DBInstanceStatus: "stopped" }] } },
      }),
    ).toBeFalsy()
  })

  it("polls the detail query while the instance is modifying", () => {
    const refetch = refetchIntervalOf(rdsDBInstanceQueryOptions("orders-db"))
    expect(
      refetch({
        state: { data: { DBInstances: [{ DBInstanceStatus: "modifying" }] } },
      }),
    ).toBe(5000)
  })

  it("stops polling the detail query once the instance is available", () => {
    const refetch = refetchIntervalOf(rdsDBInstanceQueryOptions("orders-db"))
    expect(
      refetch({
        state: { data: { DBInstances: [{ DBInstanceStatus: "available" }] } },
      }),
    ).toBeFalsy()
  })
})
