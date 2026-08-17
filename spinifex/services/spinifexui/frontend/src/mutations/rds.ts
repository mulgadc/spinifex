import {
  type Tag,
  AddTagsToResourceCommand,
  CreateDBInstanceCommand,
  DeleteDBInstanceCommand,
  ModifyDBInstanceCommand,
  RebootDBInstanceCommand,
  RemoveTagsFromResourceCommand,
  StartDBInstanceCommand,
  StopDBInstanceCommand,
} from "@aws-sdk/client-rds"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getRdsClient } from "@/lib/awsClient"
import type {
  CreateDBInstanceFormData,
  ModifyDBInstanceFormData,
} from "@/types/rds"

const DB_INSTANCES_KEY = ["rds", "dbInstances"]

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
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
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
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: DB_INSTANCES_KEY })
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
