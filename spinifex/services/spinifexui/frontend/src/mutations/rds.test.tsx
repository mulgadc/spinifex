import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

import type {
  CreateDBInstanceFormData,
  RestoreDBInstanceFormData,
} from "@/types/rds"

import {
  useCreateDBInstance,
  useCreateDBParameterGroup,
  useCreateDBSnapshot,
  useCreateDBSubnetGroup,
  useDeleteDBInstance,
  useDeleteDBParameterGroup,
  useDeleteDBSnapshot,
  useDeleteDBSubnetGroup,
  useModifyDBInstance,
  useModifyDBParameterGroup,
  useRebootDBInstance,
  useRestoreDBInstanceFromDBSnapshot,
  useStartDBInstance,
  useStopDBInstance,
  useUpdateRdsTags,
} from "./rds"

let queryClient: QueryClient

function wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function createQueryClient() {
  queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  return queryClient
}

const CREATE_FORM: CreateDBInstanceFormData = {
  dbInstanceIdentifier: "orders-db",
  engine: "postgres",
  engineVersion: "18",
  dbInstanceClass: "db.t3.micro",
  allocatedStorage: 20,
  masterUsername: "dbadmin",
  masterUserPassword: "sup3rsecret",
  confirmPassword: "sup3rsecret",
  dbName: "orders",
  port: "",
  dbSubnetGroupName: "",
  vpcSecurityGroupIds: [],
  dbParameterGroupName: "",
  deletionProtection: false,
  backupRetentionPeriod: 7,
  preferredBackupWindow: "",
  preferredMaintenanceWindow: "",
  tags: [],
}

describe("useCreateDBInstance", () => {
  it("sends CreateDBInstanceCommand with the form values", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateDBInstance(), { wrapper })

    result.current.mutate(CREATE_FORM)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBInstanceIdentifier).toBe("orders-db")
    expect(input.Engine).toBe("postgres")
    expect(input.DBInstanceClass).toBe("db.t3.micro")
    expect(input.AllocatedStorage).toBe(20)
    expect(input.MasterUsername).toBe("dbadmin")
    expect(input.DBName).toBe("orders")
    expect(input.BackupRetentionPeriod).toBe(7)
  })

  it("omits an empty port so the engine default applies", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateDBInstance(), { wrapper })

    result.current.mutate(CREATE_FORM)

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Port).toBeUndefined()
  })

  it("sends a port when one was typed", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateDBInstance(), { wrapper })

    result.current.mutate({ ...CREATE_FORM, port: "5555" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Port).toBe(5555)
  })

  it("omits empty optional strings rather than sending blanks", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateDBInstance(), { wrapper })

    result.current.mutate({ ...CREATE_FORM, dbName: "", engineVersion: "" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBName).toBeUndefined()
    expect(input.EngineVersion).toBeUndefined()
    expect(input.DBSubnetGroupName).toBeUndefined()
    expect(input.VpcSecurityGroupIds).toBeUndefined()
    expect(input.Tags).toBeUndefined()
  })
})

describe("useModifyDBInstance", () => {
  it("sends ModifyDBInstanceCommand with the identifier and ApplyImmediately", async () => {
    createQueryClient()
    const { result } = renderHook(() => useModifyDBInstance(), { wrapper })

    result.current.mutate({
      dbInstanceIdentifier: "orders-db",
      currentAllocatedStorage: 20,
      dbInstanceClass: "db.t3.small",
      allocatedStorage: 40,
      dbParameterGroupName: "custom-pg",
      vpcSecurityGroupIds: ["sg-1"],
      deletionProtection: true,
      backupRetentionPeriod: 3,
      preferredBackupWindow: "",
      preferredMaintenanceWindow: "",
      masterUserPassword: "",
      applyImmediately: true,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBInstanceIdentifier).toBe("orders-db")
    expect(input.DBInstanceClass).toBe("db.t3.small")
    expect(input.AllocatedStorage).toBe(40)
    expect(input.ApplyImmediately).toBeTruthy()
    // An unset password must not be sent as an empty reset.
    expect(input.MasterUserPassword).toBeUndefined()
  })

  it("invalidates the automated backups and snapshots as well as the instance", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useModifyDBInstance(), { wrapper })

    result.current.mutate({
      dbInstanceIdentifier: "orders-db",
      currentAllocatedStorage: 20,
      dbInstanceClass: "db.t3.micro",
      allocatedStorage: 20,
      dbParameterGroupName: "",
      vpcSecurityGroupIds: [],
      deletionProtection: false,
      backupRetentionPeriod: 0,
      preferredBackupWindow: "",
      preferredMaintenanceWindow: "",
      masterUserPassword: "",
      applyImmediately: true,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["rds", "dbInstances"] })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["rds", "automatedBackups", "orders-db"],
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["rds", "dbSnapshots"] })
  })
})

describe("useDeleteDBInstance", () => {
  it("sends a final snapshot identifier by default", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteDBInstance(), { wrapper })

    result.current.mutate({
      dbInstanceIdentifier: "orders-db",
      skipFinalSnapshot: false,
      finalSnapshotIdentifier: "orders-db-final-20260817-1200",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.SkipFinalSnapshot).toBeFalsy()
    expect(input.FinalDBSnapshotIdentifier).toBe(
      "orders-db-final-20260817-1200",
    )
  })

  it("drops the snapshot identifier when the final snapshot is skipped", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteDBInstance(), { wrapper })

    result.current.mutate({
      dbInstanceIdentifier: "orders-db",
      skipFinalSnapshot: true,
      finalSnapshotIdentifier: "orders-db-final-20260817-1200",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.SkipFinalSnapshot).toBeTruthy()
    expect(input.FinalDBSnapshotIdentifier).toBeUndefined()
  })

  // The final snapshot appears and the automated backups go, neither of which
  // is reachable from the instance list alone.
  it("invalidates the automated backups and snapshots as well as the instance", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useDeleteDBInstance(), { wrapper })

    result.current.mutate({
      dbInstanceIdentifier: "orders-db",
      skipFinalSnapshot: false,
      finalSnapshotIdentifier: "orders-db-final-20260817-1200",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["rds", "dbInstances"] })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["rds", "automatedBackups", "orders-db"],
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["rds", "dbSnapshots"] })
  })
})

describe("rds lifecycle mutations", () => {
  it.each([
    ["start", useStartDBInstance],
    ["stop", useStopDBInstance],
    ["reboot", useRebootDBInstance],
  ])("%s sends the instance identifier", async (_name, useHook) => {
    createQueryClient()
    const { result } = renderHook(() => useHook(), { wrapper })

    result.current.mutate("orders-db")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db",
    })
  })
})

describe("useUpdateRdsTags", () => {
  it("adds the final set and removes the keys that went away", async () => {
    createQueryClient()
    const { result } = renderHook(() => useUpdateRdsTags(), { wrapper })

    result.current.mutate({
      resourceName: "arn:db",
      tags: [{ key: "env", value: "prod" }],
      initialKeys: ["env", "owner"],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      ResourceName: "arn:db",
      Tags: [{ Key: "env", Value: "prod" }],
    })
    expect(mockSend.mock.calls[1]?.[0].input).toStrictEqual({
      ResourceName: "arn:db",
      TagKeys: ["owner"],
    })
  })

  it("skips both calls when there is nothing to do", async () => {
    createQueryClient()
    const { result } = renderHook(() => useUpdateRdsTags(), { wrapper })

    result.current.mutate({
      resourceName: "arn:db",
      tags: [],
      initialKeys: [],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend).not.toHaveBeenCalled()
  })
})

describe("useCreateDBSubnetGroup", () => {
  it("sends the name, description and subnets", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateDBSubnetGroup(), { wrapper })

    result.current.mutate({
      dbSubnetGroupName: "orders-subnets",
      dbSubnetGroupDescription: "Private subnets",
      subnetIds: ["subnet-1", "subnet-2"],
      tags: [{ key: "env", value: "prod" }],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBSubnetGroupName).toBe("orders-subnets")
    expect(input.DBSubnetGroupDescription).toBe("Private subnets")
    expect(input.SubnetIds).toStrictEqual(["subnet-1", "subnet-2"])
    expect(input.Tags).toStrictEqual([{ Key: "env", Value: "prod" }])
  })

  it("omits an empty tag set rather than sending a blank list", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateDBSubnetGroup(), { wrapper })

    result.current.mutate({
      dbSubnetGroupName: "orders-subnets",
      dbSubnetGroupDescription: "Private subnets",
      subnetIds: ["subnet-1"],
      tags: [],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Tags).toBeUndefined()
  })
})

describe("useCreateDBParameterGroup", () => {
  it("sends the name, family and description", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateDBParameterGroup(), {
      wrapper,
    })

    result.current.mutate({
      dbParameterGroupName: "orders-pg",
      dbParameterGroupFamily: "postgres18",
      description: "Tuned settings",
      tags: [],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBParameterGroupName).toBe("orders-pg")
    expect(input.DBParameterGroupFamily).toBe("postgres18")
    expect(input.Description).toBe("Tuned settings")
  })
})

describe("useModifyDBParameterGroup", () => {
  it("sends every edit in one request", async () => {
    createQueryClient()
    const { result } = renderHook(() => useModifyDBParameterGroup(), {
      wrapper,
    })

    result.current.mutate({
      dbParameterGroupName: "orders-pg",
      parameters: [
        {
          name: "max_connections",
          value: "200",
          applyMethod: "pending-reboot",
        },
        {
          name: "log_min_duration_statement",
          value: "500",
          applyMethod: "immediate",
        },
      ],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend).toHaveBeenCalledOnce()
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBParameterGroupName).toBe("orders-pg")
    expect(input.Parameters).toStrictEqual([
      {
        ParameterName: "max_connections",
        ParameterValue: "200",
        ApplyMethod: "pending-reboot",
      },
      {
        ParameterName: "log_min_duration_statement",
        ParameterValue: "500",
        ApplyMethod: "immediate",
      },
    ])
  })
})

describe("useCreateDBSnapshot", () => {
  it("sends the snapshot name, the instance and its tags", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateDBSnapshot(), { wrapper })

    result.current.mutate({
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      dbInstanceIdentifier: "orders-db",
      tags: [{ key: "env", value: "prod" }],
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      DBInstanceIdentifier: "orders-db",
      Tags: [{ Key: "env", Value: "prod" }],
    })
  })
})

describe("useDeleteDBSnapshot", () => {
  it("sends the snapshot identifier", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteDBSnapshot(), { wrapper })

    result.current.mutate("orders-db-snapshot-20260817-1432")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
    })
  })
})

describe("useRestoreDBInstanceFromDBSnapshot", () => {
  const RESTORE_FORM: RestoreDBInstanceFormData = {
    snapshotAllocatedStorage: 20,
    dbInstanceIdentifier: "orders-db-restored",
    dbInstanceClass: "db.t3.small",
    allocatedStorage: 40,
    port: "",
    dbSubnetGroupName: "",
    vpcSecurityGroupIds: [],
    dbParameterGroupName: "",
    deletionProtection: false,
    tags: [],
  }

  it("sends the snapshot, the new identifier and the overrides", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRestoreDBInstanceFromDBSnapshot(), {
      wrapper,
    })

    result.current.mutate({
      ...RESTORE_FORM,
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      port: "5555",
      dbSubnetGroupName: "db-subnets",
      vpcSecurityGroupIds: ["sg-1"],
      dbParameterGroupName: "orders-pg",
      deletionProtection: true,
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db-restored",
      DBSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      DBInstanceClass: "db.t3.small",
      AllocatedStorage: 40,
      Port: 5555,
      DBSubnetGroupName: "db-subnets",
      VpcSecurityGroupIds: ["sg-1"],
      DBParameterGroupName: "orders-pg",
      DeletionProtection: true,
      Tags: undefined,
    })
  })

  // Every one of these is either refused by the backend or read from the
  // snapshot's datadir, so sending one would fail the restore or lie about it.
  it("never sends the engine, the credentials or the database name", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRestoreDBInstanceFromDBSnapshot(), {
      wrapper,
    })

    result.current.mutate({
      ...RESTORE_FORM,
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.Engine).toBeUndefined()
    expect(input.EngineVersion).toBeUndefined()
    expect(input.MasterUsername).toBeUndefined()
    expect(input.MasterUserPassword).toBeUndefined()
    expect(input.DBName).toBeUndefined()
    expect(input.BackupRetentionPeriod).toBeUndefined()
    expect(input.MultiAZ).toBeUndefined()
    expect(input.PubliclyAccessible).toBeUndefined()
  })

  // An empty port means "keep the snapshot's", which is a field left off the
  // request rather than a zero.
  it("omits a blank port", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRestoreDBInstanceFromDBSnapshot(), {
      wrapper,
    })

    result.current.mutate({
      ...RESTORE_FORM,
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Port).toBeUndefined()
  })
})

describe("rds group delete mutations", () => {
  it("deleting a subnet group sends its name", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteDBSubnetGroup(), { wrapper })

    result.current.mutate("orders-subnets")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBSubnetGroupName: "orders-subnets",
    })
  })

  it("deleting a parameter group sends its name", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteDBParameterGroup(), {
      wrapper,
    })

    result.current.mutate("orders-pg")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBParameterGroupName: "orders-pg",
    })
  })
})
