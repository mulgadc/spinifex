import {
  type Tag,
  AddTagsToResourceCommand,
  CreateDBInstanceCommand,
  CreateDBParameterGroupCommand,
  CreateDBSnapshotCommand,
  CreateDBSubnetGroupCommand,
  DeleteDBInstanceCommand,
  DeleteDBParameterGroupCommand,
  DeleteDBSnapshotCommand,
  DeleteDBSubnetGroupCommand,
  ModifyDBInstanceCommand,
  ModifyDBParameterGroupCommand,
  RebootDBInstanceCommand,
  RemoveTagsFromResourceCommand,
  RestoreDBInstanceFromDBSnapshotCommand,
  StartDBInstanceCommand,
  StopDBInstanceCommand,
} from "@aws-sdk/client-rds"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getRdsClient } from "@/lib/awsClient"
import type {
  CreateDBInstanceFormData,
  CreateDBParameterGroupFormData,
  CreateDBSnapshotFormData,
  CreateDBSubnetGroupFormData,
  ModifyDBInstanceFormData,
  ParameterUpdate,
  RestoreDBInstanceFormData,
} from "@/types/rds"

const DB_INSTANCES_KEY = ["rds", "dbInstances"]
const DB_SNAPSHOTS_KEY = ["rds", "dbSnapshots"]
const AUTOMATED_BACKUPS_KEY = ["rds", "automatedBackups"]
const SUBNET_GROUPS_KEY = ["rds", "subnetGroups"]
const PARAMETER_GROUPS_KEY = ["rds", "parameterGroups"]

// An optional string field is sent only when it carries something: the backend
// rejects several parameters on sight, and an empty string is still a value.
function optional(value: string): string | undefined {
  return value.length > 0 ? value : undefined
}

function toTags(tags: { key: string; value: string }[]): Tag[] | undefined {
  const set = tags
    .filter((t) => t.key.length > 0)
    .map((t) => ({ Key: t.key, Value: t.value }))
  return set.length > 0 ? set : undefined
}

export function useCreateDBInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateDBInstanceFormData) => {
      const command = new CreateDBInstanceCommand({
        DBInstanceIdentifier: params.dbInstanceIdentifier,
        Engine: params.engine,
        EngineVersion: optional(params.engineVersion),
        DBInstanceClass: params.dbInstanceClass,
        AllocatedStorage: params.allocatedStorage,
        MasterUsername: params.masterUsername,
        MasterUserPassword: params.masterUserPassword,
        DBName: optional(params.dbName),
        Port: params.port === "" ? undefined : Number(params.port),
        DBSubnetGroupName: optional(params.dbSubnetGroupName),
        VpcSecurityGroupIds:
          params.vpcSecurityGroupIds.length > 0
            ? params.vpcSecurityGroupIds
            : undefined,
        DBParameterGroupName: optional(params.dbParameterGroupName),
        DeletionProtection: params.deletionProtection,
        BackupRetentionPeriod: params.backupRetentionPeriod,
        PreferredBackupWindow: optional(params.preferredBackupWindow),
        PreferredMaintenanceWindow: optional(params.preferredMaintenanceWindow),
        Tags: toTags(params.tags),
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
    },
  })
}

export interface ModifyDBInstanceParams extends ModifyDBInstanceFormData {
  dbInstanceIdentifier: string
}

export function useModifyDBInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ModifyDBInstanceParams) => {
      const command = new ModifyDBInstanceCommand({
        DBInstanceIdentifier: params.dbInstanceIdentifier,
        DBInstanceClass: params.dbInstanceClass,
        AllocatedStorage: params.allocatedStorage,
        DBParameterGroupName: optional(params.dbParameterGroupName),
        VpcSecurityGroupIds: params.vpcSecurityGroupIds,
        DeletionProtection: params.deletionProtection,
        BackupRetentionPeriod: params.backupRetentionPeriod,
        PreferredBackupWindow: optional(params.preferredBackupWindow),
        PreferredMaintenanceWindow: optional(params.preferredMaintenanceWindow),
        MasterUserPassword: optional(params.masterUserPassword),
        ApplyImmediately: params.applyImmediately,
      })
      return await getRdsClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
      // A retention of zero purges the instance's automated backups along with
      // the snapshots they hold, and neither is projected onto the instance.
      void queryClient.invalidateQueries({
        queryKey: [...AUTOMATED_BACKUPS_KEY, params.dbInstanceIdentifier],
      })
      void queryClient.invalidateQueries({ queryKey: DB_SNAPSHOTS_KEY })
    },
  })
}

export interface DeleteDBInstanceParams {
  dbInstanceIdentifier: string
  skipFinalSnapshot: boolean
  finalSnapshotIdentifier?: string
}

export function useDeleteDBInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: DeleteDBInstanceParams) => {
      const command = new DeleteDBInstanceCommand({
        DBInstanceIdentifier: params.dbInstanceIdentifier,
        SkipFinalSnapshot: params.skipFinalSnapshot,
        FinalDBSnapshotIdentifier: params.skipFinalSnapshot
          ? undefined
          : params.finalSnapshotIdentifier,
      })
      return await getRdsClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
      // The teardown takes the instance's automated backups with it and leaves
      // the final snapshot behind, so both snapshot views are stale too.
      void queryClient.invalidateQueries({
        queryKey: [...AUTOMATED_BACKUPS_KEY, params.dbInstanceIdentifier],
      })
      void queryClient.invalidateQueries({ queryKey: DB_SNAPSHOTS_KEY })
    },
  })
}

export function useStartDBInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (dbInstanceIdentifier: string) => {
      const command = new StartDBInstanceCommand({
        DBInstanceIdentifier: dbInstanceIdentifier,
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
    },
  })
}

export function useStopDBInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (dbInstanceIdentifier: string) => {
      const command = new StopDBInstanceCommand({
        DBInstanceIdentifier: dbInstanceIdentifier,
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
    },
  })
}

export function useRebootDBInstance() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (dbInstanceIdentifier: string) => {
      const command = new RebootDBInstanceCommand({
        DBInstanceIdentifier: dbInstanceIdentifier,
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
    },
  })
}

export interface CreateDBSnapshotParams extends CreateDBSnapshotFormData {
  dbInstanceIdentifier: string
}

export function useCreateDBSnapshot() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateDBSnapshotParams) => {
      const command = new CreateDBSnapshotCommand({
        DBSnapshotIdentifier: params.dbSnapshotIdentifier,
        DBInstanceIdentifier: params.dbInstanceIdentifier,
        Tags: toTags(params.tags),
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_SNAPSHOTS_KEY })
      // The instance passes through backing-up for the length of the snapshot,
      // so its own views are stale the moment this returns.
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
    },
  })
}

export function useDeleteDBSnapshot() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (dbSnapshotIdentifier: string) => {
      const command = new DeleteDBSnapshotCommand({
        DBSnapshotIdentifier: dbSnapshotIdentifier,
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_SNAPSHOTS_KEY })
    },
  })
}

export interface RestoreDBInstanceParams extends RestoreDBInstanceFormData {
  dbSnapshotIdentifier: string
}

// The engine, master credentials and initial database are the snapshot's and
// are never sent: the restore starts on its datadir, which already holds them.
export function useRestoreDBInstanceFromDBSnapshot() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: RestoreDBInstanceParams) => {
      const command = new RestoreDBInstanceFromDBSnapshotCommand({
        DBInstanceIdentifier: params.dbInstanceIdentifier,
        DBSnapshotIdentifier: params.dbSnapshotIdentifier,
        DBInstanceClass: params.dbInstanceClass,
        AllocatedStorage: params.allocatedStorage,
        Port: params.port === "" ? undefined : Number(params.port),
        DBSubnetGroupName: optional(params.dbSubnetGroupName),
        VpcSecurityGroupIds:
          params.vpcSecurityGroupIds.length > 0
            ? params.vpcSecurityGroupIds
            : undefined,
        DBParameterGroupName: optional(params.dbParameterGroupName),
        DeletionProtection: params.deletionProtection,
        Tags: toTags(params.tags),
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
    },
  })
}

export function useCreateDBSubnetGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateDBSubnetGroupFormData) => {
      const command = new CreateDBSubnetGroupCommand({
        DBSubnetGroupName: params.dbSubnetGroupName,
        DBSubnetGroupDescription: params.dbSubnetGroupDescription,
        SubnetIds: params.subnetIds,
        Tags: toTags(params.tags),
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: SUBNET_GROUPS_KEY })
    },
  })
}

export function useDeleteDBSubnetGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (dbSubnetGroupName: string) => {
      const command = new DeleteDBSubnetGroupCommand({
        DBSubnetGroupName: dbSubnetGroupName,
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: SUBNET_GROUPS_KEY })
    },
  })
}

export function useCreateDBParameterGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateDBParameterGroupFormData) => {
      const command = new CreateDBParameterGroupCommand({
        DBParameterGroupName: params.dbParameterGroupName,
        DBParameterGroupFamily: params.dbParameterGroupFamily,
        Description: params.description,
        Tags: toTags(params.tags),
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: PARAMETER_GROUPS_KEY })
    },
  })
}

export interface ModifyDBParameterGroupParams {
  dbParameterGroupName: string
  parameters: ParameterUpdate[]
}

// One call, never several: each ModifyDBParameterGroup is atomic on its own, so
// a save split across requests could fail partway and leave the group holding
// half the edit with nothing in the API saying so. The form enforces the cap.
export function useModifyDBParameterGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ModifyDBParameterGroupParams) => {
      const command = new ModifyDBParameterGroupCommand({
        DBParameterGroupName: params.dbParameterGroupName,
        Parameters: params.parameters.map((p) => ({
          ParameterName: p.name,
          ParameterValue: p.value,
          ApplyMethod: p.applyMethod,
        })),
      })
      return await getRdsClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["rds", "parameters", params.dbParameterGroupName],
      })
      // A modify propagates to every attached instance, moving its
      // ParameterApplyStatus, so the instance views are stale too.
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
    },
  })
}

export function useDeleteDBParameterGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (dbParameterGroupName: string) => {
      const command = new DeleteDBParameterGroupCommand({
        DBParameterGroupName: dbParameterGroupName,
      })
      return await getRdsClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: PARAMETER_GROUPS_KEY })
    },
  })
}

export interface UpdateRdsTagsParams {
  resourceName: string
  tags: { key: string; value: string }[]
  initialKeys: string[]
}

// Reconciles a DB instance's tags to the desired set: AddTagsToResource
// overwrites the final tags and RemoveTagsFromResource drops the keys that went
// away. Either call is skipped when it has no work.
export function useUpdateRdsTags() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: UpdateRdsTagsParams) => {
      const finalKeys = new Set(params.tags.map((t) => t.key))
      const toRemove = params.initialKeys.filter((k) => !finalKeys.has(k))
      const client = getRdsClient()

      if (params.tags.length > 0) {
        await client.send(
          new AddTagsToResourceCommand({
            ResourceName: params.resourceName,
            Tags: params.tags.map((t) => ({ Key: t.key, Value: t.value })),
          }),
        )
      }
      if (toRemove.length > 0) {
        await client.send(
          new RemoveTagsFromResourceCommand({
            ResourceName: params.resourceName,
            TagKeys: toRemove,
          }),
        )
      }
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["rds", "tags"] })
    },
  })
}
