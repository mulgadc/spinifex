import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

import { callQueryFn, callRefetchInterval } from "@/test/query"

import {
  rdsAutomatedBackupsQueryOptions,
  rdsDBInstanceQueryOptions,
  rdsDBInstancesQueryOptions,
  rdsDBSnapshotQueryOptions,
  rdsDBSnapshotsQueryOptions,
  rdsEngineVersionsQueryOptions,
  rdsEventsQueryOptions,
  rdsInstanceDBSnapshotsQueryOptions,
  rdsOrderableOptionsQueryOptions,
  rdsParameterGroupQueryOptions,
  rdsParameterGroupsQueryOptions,
  rdsParametersQueryOptions,
  rdsSnapshotEventsQueryOptions,
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

  it("rdsEventsQueryOptions includes the source type and identifier", () => {
    expect(rdsEventsQueryOptions("orders-db").queryKey).toStrictEqual([
      "rds",
      "events",
      "db-instance",
      "orders-db",
    ])
  })

  // The two rings are separate on the backend, so the keys have to be too: a
  // snapshot and the instance it came from can share an identifier.
  it("rdsSnapshotEventsQueryOptions keys off the snapshot source type", () => {
    expect(rdsSnapshotEventsQueryOptions("orders-db").queryKey).toStrictEqual([
      "rds",
      "events",
      "db-snapshot",
      "orders-db",
    ])
  })

  it("rdsDBSnapshotsQueryOptions has correct key", () => {
    expect(rdsDBSnapshotsQueryOptions.queryKey).toStrictEqual([
      "rds",
      "dbSnapshots",
    ])
  })

  it("rdsDBSnapshotQueryOptions includes the snapshot identifier", () => {
    expect(rdsDBSnapshotQueryOptions("orders-snap").queryKey).toStrictEqual([
      "rds",
      "dbSnapshots",
      "orders-snap",
    ])
  })

  it("rdsInstanceDBSnapshotsQueryOptions nests under the list key", () => {
    expect(
      rdsInstanceDBSnapshotsQueryOptions("orders-db").queryKey,
    ).toStrictEqual(["rds", "dbSnapshots", "instance", "orders-db"])
  })

  it("rdsAutomatedBackupsQueryOptions includes the instance identifier", () => {
    expect(rdsAutomatedBackupsQueryOptions("orders-db").queryKey).toStrictEqual(
      ["rds", "automatedBackups", "orders-db"],
    )
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

describe("rds queries send the right command", () => {
  it("dbInstances list sends an unfiltered describe", async () => {
    mockSend.mockResolvedValueOnce({ DBInstances: [] })
    await callQueryFn(rdsDBInstancesQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("dbInstance detail filters by identifier", async () => {
    mockSend.mockResolvedValueOnce({ DBInstances: [] })
    await callQueryFn(rdsDBInstanceQueryOptions("orders-db"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db",
    })
  })

  it("events asks for the whole 14-day ring, not the default hour", async () => {
    mockSend.mockResolvedValueOnce({ Events: [] })
    await callQueryFn(rdsEventsQueryOptions("orders-db"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SourceIdentifier: "orders-db",
      SourceType: "db-instance",
      Duration: 14 * 24 * 60,
    })
  })

  it("tags sends the resource name", async () => {
    mockSend.mockResolvedValueOnce({ TagList: [] })
    await callQueryFn(rdsTagsQueryOptions("arn:db"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      ResourceName: "arn:db",
    })
  })

  it("orderable options filter by engine", async () => {
    mockSend.mockResolvedValueOnce({ OrderableDBInstanceOptions: [] })
    await callQueryFn(rdsOrderableOptionsQueryOptions("mariadb"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Engine: "mariadb",
    })
  })

  it("subnet groups and parameter groups send unfiltered describes", async () => {
    mockSend.mockResolvedValueOnce({ DBSubnetGroups: [] })
    await callQueryFn(rdsSubnetGroupsQueryOptions)
    mockSend.mockResolvedValueOnce({ DBParameterGroups: [] })
    await callQueryFn(rdsParameterGroupsQueryOptions)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
    expect(mockSend.mock.calls[1]?.[0].input).toStrictEqual({})
  })

  it("the single group describes filter by name", async () => {
    mockSend.mockResolvedValueOnce({ DBSubnetGroups: [] })
    await callQueryFn(rdsSubnetGroupQueryOptions("orders-subnets"))
    mockSend.mockResolvedValueOnce({ DBParameterGroups: [] })
    await callQueryFn(rdsParameterGroupQueryOptions("orders-pg"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBSubnetGroupName: "orders-subnets",
    })
    expect(mockSend.mock.calls[1]?.[0].input).toStrictEqual({
      DBParameterGroupName: "orders-pg",
    })
  })

  it("snapshot events ask the db-snapshot ring", async () => {
    mockSend.mockResolvedValueOnce({ Events: [] })
    await callQueryFn(rdsSnapshotEventsQueryOptions("orders-snap"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      SourceIdentifier: "orders-snap",
      SourceType: "db-snapshot",
      Duration: 14 * 24 * 60,
    })
  })

  it("the snapshot describes filter by snapshot and by instance", async () => {
    mockSend.mockResolvedValueOnce({ DBSnapshots: [] })
    await callQueryFn(rdsDBSnapshotsQueryOptions)
    mockSend.mockResolvedValueOnce({ DBSnapshots: [] })
    await callQueryFn(rdsDBSnapshotQueryOptions("orders-snap"))
    mockSend.mockResolvedValueOnce({ DBSnapshots: [] })
    await callQueryFn(rdsInstanceDBSnapshotsQueryOptions("orders-db"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
    expect(mockSend.mock.calls[1]?.[0].input).toStrictEqual({
      DBSnapshotIdentifier: "orders-snap",
    })
    expect(mockSend.mock.calls[2]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db",
    })
  })

  it("automated backups filter by instance", async () => {
    mockSend.mockResolvedValueOnce({ DBInstanceAutomatedBackups: [] })
    await callQueryFn(rdsAutomatedBackupsQueryOptions("orders-db"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db",
    })
  })

  // The Source filter the backend accepts is deliberately unused: the whole
  // set is one round trip and the split is a client-side filter.
  it("parameters are fetched unfiltered by source", async () => {
    mockSend.mockResolvedValueOnce({ Parameters: [] })
    await callQueryFn(rdsParametersQueryOptions("orders-pg"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBParameterGroupName: "orders-pg",
    })
  })
})

describe("rds poll cadence", () => {
  it("polls the list while any instance is creating", () => {
    expect(
      callRefetchInterval(rdsDBInstancesQueryOptions, {
        DBInstances: [{ DBInstanceStatus: "creating" }],
      }),
    ).toBe(5000)
  })

  it("stops polling the list once every instance is available", () => {
    expect(
      callRefetchInterval(rdsDBInstancesQueryOptions, {
        DBInstances: [{ DBInstanceStatus: "available" }],
      }),
    ).toBeFalsy()
  })

  it("does not treat a stopped instance as in flight", () => {
    expect(
      callRefetchInterval(rdsDBInstancesQueryOptions, {
        DBInstances: [{ DBInstanceStatus: "stopped" }],
      }),
    ).toBeFalsy()
  })

  it("polls the detail query while the instance is modifying", () => {
    expect(
      callRefetchInterval(rdsDBInstanceQueryOptions("orders-db"), {
        DBInstances: [{ DBInstanceStatus: "modifying" }],
      }),
    ).toBe(5000)
  })

  it("stops polling the detail query once the instance is available", () => {
    expect(
      callRefetchInterval(rdsDBInstanceQueryOptions("orders-db"), {
        DBInstances: [{ DBInstanceStatus: "available" }],
      }),
    ).toBeFalsy()
  })

  it("polls the snapshot list while one is still being taken", () => {
    expect(
      callRefetchInterval(rdsDBSnapshotsQueryOptions, {
        DBSnapshots: [{ Status: "creating" }],
      }),
    ).toBe(5000)
  })

  it("stops polling the snapshot list once every snapshot is available", () => {
    expect(
      callRefetchInterval(rdsDBSnapshotsQueryOptions, {
        DBSnapshots: [{ Status: "available" }],
      }),
    ).toBeFalsy()
  })

  it("polls an instance's snapshots while one is still being taken", () => {
    expect(
      callRefetchInterval(rdsInstanceDBSnapshotsQueryOptions("orders-db"), {
        DBSnapshots: [{ Status: "creating" }],
      }),
    ).toBe(5000)
  })
})
