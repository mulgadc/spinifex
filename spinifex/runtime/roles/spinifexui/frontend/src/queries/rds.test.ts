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

describe("rds query key relations", () => {
  // The two event rings are separate on the backend, so the keys must be too:
  // a snapshot and the instance it came from can share an identifier.
  it("keeps instance and snapshot events apart for a shared identifier", () => {
    expect(
      rdsSnapshotEventsQueryOptions("orders-db").queryKey,
    ).not.toStrictEqual(rdsEventsQueryOptions("orders-db").queryKey)
  })

  it("nests an instance's snapshots under the snapshot list key", () => {
    expect(
      rdsInstanceDBSnapshotsQueryOptions("orders-db").queryKey.slice(
        0,
        rdsDBSnapshotsQueryOptions.queryKey.length,
      ),
    ).toStrictEqual(rdsDBSnapshotsQueryOptions.queryKey)
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
