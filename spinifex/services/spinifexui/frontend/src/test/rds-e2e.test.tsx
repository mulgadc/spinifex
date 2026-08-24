// Cross-slice RDS flow against a mocked SDK. Walks the path a user takes:
// create → poll through `creating` to `available` → modify → delete with a
// final snapshot, all through the same mocked dispatcher.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook } from "@testing-library/react"
import type { ReactNode } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

interface StoredInstance {
  identifier: string
  engine: string
  engineVersion?: string
  instanceClass: string
  allocatedStorage: number
  masterUsername: string
  dbName?: string
  port: number
  status: string
  deletionProtection: boolean
  backupRetentionPeriod: number
  describesLeftAsCreating: number
  restoredFrom?: string
  pending: { instanceClass?: string; allocatedStorage?: number }
}

interface StoredSnapshot {
  identifier: string
  sourceIdentifier: string
  engine: string
  engineVersion: string
  allocatedStorage: number
  masterUsername: string
  port: number
  status: string
  snapshotType: string
  describesLeftAsCreating: number
}

interface Command {
  readonly constructor: { name: string }
  readonly input: unknown
}

function reject(input: unknown, names: string[], action: string) {
  const record = input as Record<string, unknown>
  for (const name of names) {
    if (record[name] !== undefined) {
      throw new Error(`InvalidParameterValue: ${action} rejects ${name}`)
    }
  }
}

function project(instance: StoredInstance) {
  return {
    DBInstanceIdentifier: instance.identifier,
    DBInstanceArn: `arn:aws:rds:ap-southeast-2:000000000000:db:${instance.identifier}`,
    DBInstanceStatus: instance.status,
    Engine: instance.engine,
    EngineVersion: instance.engineVersion ?? "18",
    DBInstanceClass: instance.instanceClass,
    AllocatedStorage: instance.allocatedStorage,
    StorageType: "gp3",
    StorageEncrypted: true,
    MasterUsername: instance.masterUsername,
    DBName: instance.dbName,
    DeletionProtection: instance.deletionProtection,
    BackupRetentionPeriod: instance.backupRetentionPeriod,
    Endpoint: {
      Address: `${instance.identifier}.rds.internal`,
      Port: instance.port,
    },
    PendingModifiedValues: {
      DBInstanceClass: instance.pending.instanceClass,
      AllocatedStorage: instance.pending.allocatedStorage,
    },
  }
}

function projectSnapshot(snapshot: StoredSnapshot) {
  return {
    DBSnapshotIdentifier: snapshot.identifier,
    DBSnapshotArn: `arn:aws:rds:ap-southeast-2:000000000000:snapshot:${snapshot.identifier}`,
    DBInstanceIdentifier: snapshot.sourceIdentifier,
    SnapshotType: snapshot.snapshotType,
    Status: snapshot.status,
    Engine: snapshot.engine,
    EngineVersion: snapshot.engineVersion,
    AllocatedStorage: snapshot.allocatedStorage,
    MasterUsername: snapshot.masterUsername,
    Port: snapshot.port,
    Encrypted: true,
  }
}

const { sdk } = vi.hoisted(() => {
  // The parameters CreateDBInstance rejects outright. The mock refuses them the
  // way the backend does, so a form that starts sending one fails the flow.
  const REJECTED_ON_CREATE = [
    "MultiAZ",
    "PubliclyAccessible",
    "Iops",
    "MaxAllocatedStorage",
    "StorageThroughput",
    "KmsKeyId",
    "AvailabilityZone",
    "DBSecurityGroups",
    "DBClusterIdentifier",
    "EnableIAMDatabaseAuthentication",
    "EnableCloudwatchLogsExports",
  ]

  const REJECTED_ON_MODIFY = [
    "NewDBInstanceIdentifier",
    "Engine",
    "EngineVersion",
    "DBPortNumber",
    "DBSubnetGroupName",
    "MultiAZ",
    "PubliclyAccessible",
    "OptionGroupName",
  ]

  // Everything the restore reads from the snapshot's datadir instead, which
  // the backend refuses rather than silently ignores.
  const REJECTED_ON_RESTORE = [
    "Engine",
    "EngineVersion",
    "MasterUsername",
    "MasterUserPassword",
    "DBName",
    "BackupRetentionPeriod",
    "MultiAZ",
    "PubliclyAccessible",
  ]

  // Two describes report `creating` before the instance settles, so the poll
  // path is exercised rather than the instance appearing available at once.
  const DESCRIBES_WHILE_CREATING = 2
  // oxlint-disable-next-line anti-slop/no-known-value-widening -- keyed by the engine the command names, not by a closed set
  const ENGINE_PORTS: Record<string, number> = { postgres: 5432, mariadb: 3306 }

  const state = {
    instances: [] as StoredInstance[],
    snapshots: [] as StoredSnapshot[],
  }

  function find(identifier: string): StoredInstance {
    const instance = state.instances.find((i) => i.identifier === identifier)
    if (!instance) {
      throw new Error(`DBInstanceNotFound: ${identifier}`)
    }
    return instance
  }

  function findSnapshot(identifier: string): StoredSnapshot {
    const snapshot = state.snapshots.find((s) => s.identifier === identifier)
    if (!snapshot) {
      throw new Error(`DBSnapshotNotFound: ${identifier}`)
    }
    return snapshot
  }

  function snapshotOf(
    instance: StoredInstance,
    identifier: string,
    snapshotType: string,
  ): StoredSnapshot {
    return {
      identifier,
      sourceIdentifier: instance.identifier,
      engine: instance.engine,
      engineVersion: instance.engineVersion ?? "18",
      allocatedStorage: instance.allocatedStorage,
      masterUsername: instance.masterUsername,
      port: instance.port,
      status: "creating",
      snapshotType,
      describesLeftAsCreating: DESCRIBES_WHILE_CREATING,
    }
  }

  const handlers = new Map<string, (i: never) => unknown>([
    [
      "CreateDBInstanceCommand",
      (i: {
        DBInstanceIdentifier: string
        Engine: string
        EngineVersion?: string
        DBInstanceClass: string
        AllocatedStorage: number
        MasterUsername: string
        DBName?: string
        Port?: number
        DeletionProtection?: boolean
        BackupRetentionPeriod?: number
      }) => {
        reject(i, REJECTED_ON_CREATE, "CreateDBInstance")
        if (
          state.instances.some((s) => s.identifier === i.DBInstanceIdentifier)
        ) {
          throw new Error(`DBInstanceAlreadyExists: ${i.DBInstanceIdentifier}`)
        }
        state.instances.push({
          identifier: i.DBInstanceIdentifier,
          engine: i.Engine,
          engineVersion: i.EngineVersion,
          instanceClass: i.DBInstanceClass,
          allocatedStorage: i.AllocatedStorage,
          masterUsername: i.MasterUsername,
          dbName: i.DBName,
          port: i.Port ?? ENGINE_PORTS[i.Engine] ?? 5432,
          status: "creating",
          deletionProtection: i.DeletionProtection ?? false,
          backupRetentionPeriod: i.BackupRetentionPeriod ?? 7,
          describesLeftAsCreating: DESCRIBES_WHILE_CREATING,
          pending: {},
        })
        return { DBInstance: project(find(i.DBInstanceIdentifier)) }
      },
    ],
    [
      "DescribeDBInstancesCommand",
      (i: { DBInstanceIdentifier?: string }) => {
        const matching = i.DBInstanceIdentifier
          ? [find(i.DBInstanceIdentifier)]
          : state.instances
        const projected = matching.map(project)
        // Advance after projecting, so the first describe still reads `creating`.
        for (const instance of matching) {
          if (instance.status === "creating") {
            instance.describesLeftAsCreating -= 1
            if (instance.describesLeftAsCreating <= 0) {
              instance.status = "available"
            }
          } else if (instance.status === "modifying") {
            instance.status = "available"
            instance.instanceClass =
              instance.pending.instanceClass ?? instance.instanceClass
            instance.allocatedStorage =
              instance.pending.allocatedStorage ?? instance.allocatedStorage
            instance.pending = {}
          }
        }
        return { DBInstances: projected }
      },
    ],
    [
      "ModifyDBInstanceCommand",
      (i: {
        DBInstanceIdentifier: string
        DBInstanceClass?: string
        AllocatedStorage?: number
        DeletionProtection?: boolean
        BackupRetentionPeriod?: number
      }) => {
        reject(i, REJECTED_ON_MODIFY, "ModifyDBInstance")
        const instance = find(i.DBInstanceIdentifier)
        if (
          i.AllocatedStorage !== undefined &&
          i.AllocatedStorage < instance.allocatedStorage
        ) {
          throw new Error("InvalidParameterValue: storage cannot be shrunk")
        }
        instance.status = "modifying"
        instance.pending = {
          instanceClass: i.DBInstanceClass,
          allocatedStorage: i.AllocatedStorage,
        }
        if (i.DeletionProtection !== undefined) {
          instance.deletionProtection = i.DeletionProtection
        }
        if (i.BackupRetentionPeriod !== undefined) {
          instance.backupRetentionPeriod = i.BackupRetentionPeriod
        }
        return { DBInstance: project(instance) }
      },
    ],
    [
      "DeleteDBInstanceCommand",
      (i: {
        DBInstanceIdentifier: string
        SkipFinalSnapshot?: boolean
        FinalDBSnapshotIdentifier?: string
      }) => {
        const instance = find(i.DBInstanceIdentifier)
        if (instance.deletionProtection) {
          throw new Error("InvalidParameterCombination: deletion protection")
        }
        if (i.SkipFinalSnapshot !== true) {
          if (!i.FinalDBSnapshotIdentifier) {
            throw new Error(
              "InvalidParameterCombination: FinalDBSnapshotIdentifier required",
            )
          }
          state.snapshots.push(
            snapshotOf(instance, i.FinalDBSnapshotIdentifier, "manual"),
          )
        }
        instance.status = "deleting"
        state.instances = state.instances.filter(
          (s) => s.identifier !== instance.identifier,
        )
        return { DBInstance: project(instance) }
      },
    ],
    [
      "CreateDBSnapshotCommand",
      (i: { DBSnapshotIdentifier: string; DBInstanceIdentifier: string }) => {
        if (i.DBSnapshotIdentifier.startsWith("rds:")) {
          throw new Error("InvalidParameterValue: rds: is reserved")
        }
        const instance = find(i.DBInstanceIdentifier)
        if (instance.status !== "available" && instance.status !== "stopped") {
          throw new Error(
            `InvalidDBInstanceState: ${instance.identifier} is ${instance.status}`,
          )
        }
        const snapshot = snapshotOf(instance, i.DBSnapshotIdentifier, "manual")
        state.snapshots.push(snapshot)
        return { DBSnapshot: projectSnapshot(snapshot) }
      },
    ],
    [
      "DescribeDBSnapshotsCommand",
      (i: { DBSnapshotIdentifier?: string; DBInstanceIdentifier?: string }) => {
        let matching = state.snapshots
        if (i.DBSnapshotIdentifier) {
          matching = [findSnapshot(i.DBSnapshotIdentifier)]
        } else if (i.DBInstanceIdentifier) {
          matching = matching.filter(
            (s) => s.sourceIdentifier === i.DBInstanceIdentifier,
          )
        }
        const projected = matching.map(projectSnapshot)
        // Advance after projecting, so the first describe still reads `creating`.
        for (const snapshot of matching) {
          if (snapshot.status !== "creating") {
            continue
          }
          snapshot.describesLeftAsCreating -= 1
          if (snapshot.describesLeftAsCreating <= 0) {
            snapshot.status = "available"
          }
        }
        return { DBSnapshots: projected }
      },
    ],
    [
      "DeleteDBSnapshotCommand",
      (i: { DBSnapshotIdentifier: string }) => {
        if (i.DBSnapshotIdentifier.startsWith("rds:")) {
          throw new Error("InvalidParameterValue: rds: is reserved")
        }
        const snapshot = findSnapshot(i.DBSnapshotIdentifier)
        if (snapshot.status !== "available") {
          throw new Error(
            `InvalidDBSnapshotState: ${snapshot.identifier} is ${snapshot.status}`,
          )
        }
        const reader = state.instances.find(
          (s) => s.restoredFrom === snapshot.identifier,
        )
        if (reader) {
          throw new Error(
            `InvalidDBSnapshotState: ${snapshot.identifier} is in use by ${reader.identifier}`,
          )
        }
        state.snapshots = state.snapshots.filter(
          (s) => s.identifier !== snapshot.identifier,
        )
        return { DBSnapshot: projectSnapshot(snapshot) }
      },
    ],
    [
      "RestoreDBInstanceFromDBSnapshotCommand",
      (i: {
        DBInstanceIdentifier: string
        DBSnapshotIdentifier: string
        DBInstanceClass: string
        AllocatedStorage?: number
        Port?: number
        DeletionProtection?: boolean
      }) => {
        reject(i, REJECTED_ON_RESTORE, "RestoreDBInstanceFromDBSnapshot")
        const snapshot = findSnapshot(i.DBSnapshotIdentifier)
        if (snapshot.status !== "available") {
          throw new Error(
            `InvalidDBSnapshotState: ${snapshot.identifier} is ${snapshot.status}`,
          )
        }
        if (
          state.instances.some((s) => s.identifier === i.DBInstanceIdentifier)
        ) {
          throw new Error(`DBInstanceAlreadyExists: ${i.DBInstanceIdentifier}`)
        }
        const storage = i.AllocatedStorage ?? snapshot.allocatedStorage
        if (storage < snapshot.allocatedStorage) {
          throw new Error("InvalidParameterValue: storage cannot be shrunk")
        }
        state.instances.push({
          identifier: i.DBInstanceIdentifier,
          engine: snapshot.engine,
          engineVersion: snapshot.engineVersion,
          instanceClass: i.DBInstanceClass,
          allocatedStorage: storage,
          masterUsername: snapshot.masterUsername,
          port: i.Port ?? snapshot.port,
          status: "creating",
          deletionProtection: i.DeletionProtection ?? false,
          backupRetentionPeriod: 7,
          describesLeftAsCreating: DESCRIBES_WHILE_CREATING,
          restoredFrom: snapshot.identifier,
          pending: {},
        })
        return { DBInstance: project(find(i.DBInstanceIdentifier)) }
      },
    ],
  ])

  const send = vi.fn(async (command: Command): Promise<unknown> => {
    const handler = handlers.get(command.constructor.name)
    if (!handler) {
      throw new Error(
        `No E2E handler for SDK command ${command.constructor.name}`,
      )
    }
    return handler(command.input as never)
  })

  return {
    sdk: {
      send,
      snapshots: () => state.snapshots,
      reset: () => {
        state.instances = []
        state.snapshots = []
        send.mockClear()
      },
    },
  }
})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: sdk.send }),
  getEc2Client: () => ({ send: sdk.send }),
}))

import {
  useCreateDBInstance,
  useCreateDBSnapshot,
  useDeleteDBInstance,
  useDeleteDBSnapshot,
  useModifyDBInstance,
  useRestoreDBInstanceFromDBSnapshot,
} from "@/mutations/rds"
import {
  rdsDBInstanceQueryOptions,
  rdsDBInstancesQueryOptions,
  rdsDBSnapshotQueryOptions,
  rdsDBSnapshotsQueryOptions,
  rdsInstanceDBSnapshotsQueryOptions,
} from "@/queries/rds"
import type { CreateDBInstanceFormData } from "@/types/rds"

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

function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0, staleTime: 0 },
      mutations: { retry: false },
    },
  })
}

function Harness() {
  return {
    create: useCreateDBInstance(),
    modify: useModifyDBInstance(),
    remove: useDeleteDBInstance(),
    snapshot: useCreateDBSnapshot(),
    restore: useRestoreDBInstanceFromDBSnapshot(),
    removeSnapshot: useDeleteDBSnapshot(),
  }
}

function renderHarness(qc: QueryClient) {
  return renderHook(Harness, {
    wrapper: ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    ),
  })
}

async function statusOf(qc: QueryClient): Promise<string | undefined> {
  const data = await qc.fetchQuery(rdsDBInstanceQueryOptions("orders-db"))
  return data.DBInstances?.[0]?.DBInstanceStatus
}

describe("RDS cross-slice flow (mocked SDK)", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("creates → polls to available → modifies → deletes with a final snapshot", async () => {
    const qc = createQueryClient()
    const { result } = renderHarness(qc)

    await result.current.create.mutateAsync(CREATE_FORM)

    const listed = await qc.fetchQuery(rdsDBInstancesQueryOptions)
    expect(listed.DBInstances).toHaveLength(1)
    expect(listed.DBInstances?.[0]?.DBInstanceStatus).toBe("creating")
    expect(listed.DBInstances?.[0]?.Endpoint?.Port).toBe(5432)

    // The poll the conditional refetchInterval drives, run by hand.
    expect(await statusOf(qc)).toBe("creating")
    expect(await statusOf(qc)).toBe("available")

    await result.current.modify.mutateAsync({
      dbInstanceIdentifier: "orders-db",
      currentAllocatedStorage: 20,
      dbInstanceClass: "db.t3.small",
      allocatedStorage: 40,
      dbParameterGroupName: "",
      vpcSecurityGroupIds: [],
      deletionProtection: false,
      backupRetentionPeriod: 3,
      preferredBackupWindow: "",
      preferredMaintenanceWindow: "",
      masterUserPassword: "",
      applyImmediately: true,
    })

    const modifying = await qc.fetchQuery(
      rdsDBInstanceQueryOptions("orders-db"),
    )
    expect(modifying.DBInstances?.[0]?.DBInstanceStatus).toBe("modifying")
    expect(
      modifying.DBInstances?.[0]?.PendingModifiedValues?.AllocatedStorage,
    ).toBe(40)

    const settled = await qc.fetchQuery(rdsDBInstanceQueryOptions("orders-db"))
    expect(settled.DBInstances?.[0]?.DBInstanceStatus).toBe("available")
    expect(settled.DBInstances?.[0]?.AllocatedStorage).toBe(40)
    expect(settled.DBInstances?.[0]?.DBInstanceClass).toBe("db.t3.small")

    await result.current.remove.mutateAsync({
      dbInstanceIdentifier: "orders-db",
      skipFinalSnapshot: false,
      finalSnapshotIdentifier: "orders-db-final-20260817-1432",
    })

    expect(sdk.snapshots()).toHaveLength(1)
    expect(sdk.snapshots()[0]).toMatchObject({
      identifier: "orders-db-final-20260817-1432",
      sourceIdentifier: "orders-db",
    })
    const remaining = await qc.fetchQuery(rdsDBInstancesQueryOptions)
    expect(remaining.DBInstances).toHaveLength(0)
  })

  it("never sends a parameter the backend rejects on create", async () => {
    const qc = createQueryClient()
    const { result } = renderHarness(qc)

    await expect(
      result.current.create.mutateAsync(CREATE_FORM),
    ).resolves.toBeDefined()
  })

  it("refuses to shrink storage the way the backend does", async () => {
    const qc = createQueryClient()
    const { result } = renderHarness(qc)

    await result.current.create.mutateAsync({
      ...CREATE_FORM,
      allocatedStorage: 40,
    })

    await expect(
      result.current.modify.mutateAsync({
        dbInstanceIdentifier: "orders-db",
        currentAllocatedStorage: 40,
        dbInstanceClass: "db.t3.micro",
        allocatedStorage: 20,
        dbParameterGroupName: "",
        vpcSecurityGroupIds: [],
        deletionProtection: false,
        backupRetentionPeriod: 7,
        preferredBackupWindow: "",
        preferredMaintenanceWindow: "",
        masterUserPassword: "",
        applyImmediately: true,
      }),
    ).rejects.toThrow(/storage cannot be shrunk/)
  })

  it("refuses to delete an instance with deletion protection on", async () => {
    const qc = createQueryClient()
    const { result } = renderHarness(qc)

    await result.current.create.mutateAsync({
      ...CREATE_FORM,
      deletionProtection: true,
    })

    await expect(
      result.current.remove.mutateAsync({
        dbInstanceIdentifier: "orders-db",
        skipFinalSnapshot: true,
        finalSnapshotIdentifier: "",
      }),
    ).rejects.toThrow(/deletion protection/)
  })

  it("keeps no snapshot when the final snapshot is skipped", async () => {
    const qc = createQueryClient()
    const { result } = renderHarness(qc)

    await result.current.create.mutateAsync(CREATE_FORM)
    await result.current.remove.mutateAsync({
      dbInstanceIdentifier: "orders-db",
      skipFinalSnapshot: true,
      finalSnapshotIdentifier: "orders-db-final-20260817-1432",
    })

    expect(sdk.snapshots()).toHaveLength(0)
  })
})

const RESTORE_FORM = {
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

// The poll the conditional refetchInterval drives, run by hand until the
// snapshot settles.
async function settledSnapshot(qc: QueryClient, identifier: string) {
  await qc.fetchQuery(rdsDBSnapshotQueryOptions(identifier))
  await qc.fetchQuery(rdsDBSnapshotQueryOptions(identifier))
  return await qc.fetchQuery(rdsDBSnapshotQueryOptions(identifier))
}

describe("RDS snapshot flow (mocked SDK)", () => {
  beforeEach(() => {
    sdk.reset()
  })

  async function settledInstance(qc: QueryClient) {
    const { result } = renderHarness(qc)
    await result.current.create.mutateAsync(CREATE_FORM)
    await statusOf(qc)
    await statusOf(qc)
    return result
  }

  it("snapshots → polls to available → restores → deletes the snapshot", async () => {
    const qc = createQueryClient()
    const result = await settledInstance(qc)

    await result.current.snapshot.mutateAsync({
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      dbInstanceIdentifier: "orders-db",
      tags: [],
    })

    const listed = await qc.fetchQuery(rdsDBSnapshotsQueryOptions)
    expect(listed.DBSnapshots?.[0]?.Status).toBe("creating")
    expect(listed.DBSnapshots?.[0]?.Engine).toBe("postgres")

    const settled = await settledSnapshot(
      qc,
      "orders-db-snapshot-20260817-1432",
    )
    expect(settled.DBSnapshots?.[0]?.Status).toBe("available")
    expect(settled.DBSnapshots?.[0]?.AllocatedStorage).toBe(20)

    await result.current.restore.mutateAsync({
      ...RESTORE_FORM,
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
    })

    const restored = await qc.fetchQuery(
      rdsDBInstanceQueryOptions("orders-db-restored"),
    )
    // The engine, the master user and the port come from the snapshot; only
    // the class and the grown storage came from the form.
    expect(restored.DBInstances?.[0]?.Engine).toBe("postgres")
    expect(restored.DBInstances?.[0]?.MasterUsername).toBe("dbadmin")
    expect(restored.DBInstances?.[0]?.Endpoint?.Port).toBe(5432)
    expect(restored.DBInstances?.[0]?.DBInstanceClass).toBe("db.t3.small")
    expect(restored.DBInstances?.[0]?.AllocatedStorage).toBe(40)

    // The restored instance still reads through the snapshot, so the delete is
    // refused until it is gone.
    await expect(
      result.current.removeSnapshot.mutateAsync(
        "orders-db-snapshot-20260817-1432",
      ),
    ).rejects.toThrow(/in use by orders-db-restored/)

    await result.current.remove.mutateAsync({
      dbInstanceIdentifier: "orders-db-restored",
      skipFinalSnapshot: true,
      finalSnapshotIdentifier: "",
    })
    await result.current.removeSnapshot.mutateAsync(
      "orders-db-snapshot-20260817-1432",
    )
    expect(sdk.snapshots()).toHaveLength(0)
  })

  it("lists an instance's own snapshots", async () => {
    const qc = createQueryClient()
    const result = await settledInstance(qc)

    await result.current.snapshot.mutateAsync({
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      dbInstanceIdentifier: "orders-db",
      tags: [],
    })

    const mine = await qc.fetchQuery(
      rdsInstanceDBSnapshotsQueryOptions("orders-db"),
    )
    expect(mine.DBSnapshots).toHaveLength(1)
    const others = await qc.fetchQuery(
      rdsInstanceDBSnapshotsQueryOptions("billing-db"),
    )
    expect(others.DBSnapshots).toHaveLength(0)
  })

  it("refuses a snapshot of an instance still being created", async () => {
    const qc = createQueryClient()
    const { result } = renderHarness(qc)
    await result.current.create.mutateAsync(CREATE_FORM)

    await expect(
      result.current.snapshot.mutateAsync({
        dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
        dbInstanceIdentifier: "orders-db",
        tags: [],
      }),
    ).rejects.toThrow(/is creating/)
  })

  it("refuses a restore that would shrink the volume", async () => {
    const qc = createQueryClient()
    const result = await settledInstance(qc)

    await result.current.snapshot.mutateAsync({
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      dbInstanceIdentifier: "orders-db",
      tags: [],
    })
    await settledSnapshot(qc, "orders-db-snapshot-20260817-1432")

    await expect(
      result.current.restore.mutateAsync({
        ...RESTORE_FORM,
        allocatedStorage: 10,
        dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      }),
    ).rejects.toThrow(/storage cannot be shrunk/)
  })

  // The restore reads the engine and the credentials from the datadir, so
  // sending either is a request the backend refuses outright.
  it("never sends what the restore reads from the snapshot", async () => {
    const qc = createQueryClient()
    const result = await settledInstance(qc)

    await result.current.snapshot.mutateAsync({
      dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      dbInstanceIdentifier: "orders-db",
      tags: [],
    })
    await settledSnapshot(qc, "orders-db-snapshot-20260817-1432")

    await expect(
      result.current.restore.mutateAsync({
        ...RESTORE_FORM,
        dbSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
      }),
    ).resolves.toBeDefined()
  })
})
