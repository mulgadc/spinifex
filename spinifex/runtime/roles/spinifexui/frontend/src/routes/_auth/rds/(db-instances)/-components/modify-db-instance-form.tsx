import type { DBInstance } from "@aws-sdk/client-rds"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import type { SubmitEvent } from "react"
import { type DeepPartial, useForm, useWatch } from "react-hook-form"

import {
  type CliCommand,
  CliCommandPanel,
  commandFlag,
  optionalFlag,
  variableFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { SelectField, selectOptions, TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { pickerNotice } from "@/components/picker-notice"
import { FieldDescription } from "@/components/ui/field"
import { useModifyDBInstance } from "@/mutations/rds"
import { ec2SecurityGroupsQueryOptions } from "@/queries/ec2"
import {
  rdsEngineVersionsQueryOptions,
  rdsOrderableOptionsQueryOptions,
  rdsParameterGroupsQueryOptions,
} from "@/queries/rds"
import {
  type ModifyDBInstanceFormData,
  MAX_BACKUP_RETENTION_DAYS,
  modifyDBInstanceSchema,
} from "@/types/rds"

import {
  DeletionProtectionField,
  FieldNotes,
  instanceClassOptions,
  parameterGroupOptions,
  parameterGroupsForEngine,
} from "../../-components/rds-form-fields"
import {
  BackupWindowField,
  MaintenanceWindowField,
} from "../../-components/window-fields"
import {
  ApplyImmediatelyField,
  currentSecurityGroupIds,
  InstanceSecurityGroupsField,
  modifyDefaults,
  securityGroupsForInstance,
} from "./modify-db-instance-fields"

// ModifyDBInstance refuses these outright, and they are the ones users reach
// for first, so they are shown read-only rather than left unexplained.
const READ_ONLY_FIELDS: { label: string; note: string }[] = [
  {
    label: "Identifier",
    note: "The identifier is the endpoint hostname and the certificate subject; renaming in place is not implemented.",
  },
  {
    label: "Engine version",
    note: "There is no in-place engine-version upgrade.",
  },
  {
    label: "Port",
    note: "The port is fixed at create; changing it would break every client and the serving certificate.",
  },
  {
    label: "DB subnet group",
    note: "The endpoint ENI is placed at create; moving it would change the address clients resolve.",
  },
]

interface ModifyDBInstanceFormProps {
  onClose: () => void
  instance: DBInstance
}

export function ModifyDBInstanceForm({
  onClose,
  instance,
}: ModifyDBInstanceFormProps) {
  const modifyInstance = useModifyDBInstance()
  const parameterGroupsQuery = useQuery(rdsParameterGroupsQueryOptions)
  const securityGroupsQuery = useQuery(ec2SecurityGroupsQueryOptions)
  const engineVersionsQuery = useQuery(rdsEngineVersionsQueryOptions)

  const orderableQuery = useQuery(
    rdsOrderableOptionsQueryOptions(instance.Engine ?? ""),
  )

  const identifier = instance.DBInstanceIdentifier ?? ""
  const currentStorage = instance.AllocatedStorage ?? 0

  const currentGroups = currentSecurityGroupIds(instance)

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
  } = useForm<ModifyDBInstanceFormData>({
    resolver: zodResolver(modifyDBInstanceSchema),
    defaultValues: modifyDefaults(instance, currentGroups),
  })

  const values = useWatch({ control })
  const selectedClass = values.dbInstanceClass ?? ""
  const selectedStorage = values.allocatedStorage ?? currentStorage
  const selectedGroups = values.vpcSecurityGroupIds ?? []

  const { instanceClasses, classNotice } = instanceClassOptions(
    orderableQuery,
    instance.Engine ?? "",
  )

  const parameterGroups = parameterGroupsForEngine(
    parameterGroupsQuery.data?.DBParameterGroups ?? [],
    engineVersionsQuery.data?.DBEngineVersions ?? [],
    instance.Engine ?? "",
  )

  const securityGroups = securityGroupsForInstance(
    securityGroupsQuery.data?.SecurityGroups,
    instance,
  )

  const classChanged = selectedClass !== instance.DBInstanceClass
  const storageChanged = selectedStorage !== currentStorage

  const parameterGroupNotice = pickerNotice(
    parameterGroupsQuery,
    parameterGroups.length === 0,
    {
      loading: "Loading the parameter groups…",
      failed: "Could not read the parameter groups",
      empty: "No parameter group of this instance's engine family exists.",
    },
  )

  const securityGroupNotice = pickerNotice(
    securityGroupsQuery,
    securityGroups.length === 0,
    {
      loading: "Loading the security groups…",
      failed: "Could not read the security groups",
      empty: "No security group in this instance's VPC is available.",
    },
  )

  const setSecurityGroups = (next: string[]) => {
    setValue("vpcSecurityGroupIds", next, { shouldValidate: true })
  }

  const onSubmit = async (data: ModifyDBInstanceFormData) => {
    try {
      await modifyInstance.mutateAsync({
        ...data,
        dbInstanceIdentifier: identifier,
      })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  const submit = (event: SubmitEvent<HTMLFormElement>) => {
    event.stopPropagation()
    void handleSubmit(onSubmit)(event)
  }

  return (
    <FormPanel onClose={onClose} title={`Modify ${identifier}`}>
      <form onSubmit={submit}>
        <FormLock className="space-y-4" locked={modifyInstance.isPending}>
          <FieldDescription>
            Changes apply in the maintenance window unless you apply them
            immediately.
          </FieldDescription>

          <SelectField
            control={control}
            id="modify-class"
            label="DB instance class"
            name="dbInstanceClass"
            notice={classNotice}
            options={selectOptions(instanceClasses)}
            placeholder="Select an instance class"
            warning={
              classChanged &&
              "Changing the class replaces the VM. The database is unavailable while it does."
            }
          />

          <TextField
            description={`Currently ${currentStorage} GiB. Storage grows only.`}
            error={errors.allocatedStorage}
            id="modify-storage"
            label="Allocated storage (GiB)"
            min={currentStorage}
            type="number"
            warning={
              storageChanged &&
              "Growing storage stops and starts the instance to extend the filesystem."
            }
            {...register("allocatedStorage", { valueAsNumber: true })}
          />

          <SelectField
            control={control}
            id="modify-parameter-group"
            label="DB parameter group"
            name="dbParameterGroupName"
            notice={parameterGroupNotice}
            options={parameterGroupOptions(parameterGroups)}
            placeholder="Engine default group"
          />

          <InstanceSecurityGroupsField
            groups={securityGroups}
            notice={securityGroupNotice}
            onChange={setSecurityGroups}
            selected={selectedGroups}
          />

          <TextField
            error={errors.backupRetentionPeriod}
            id="modify-retention"
            label="Backup retention (days)"
            max={MAX_BACKUP_RETENTION_DAYS}
            min={0}
            type="number"
            {...register("backupRetentionPeriod", { valueAsNumber: true })}
          />

          <BackupWindowField
            control={control}
            id="modify-backup-window"
            name="preferredBackupWindow"
          />

          <MaintenanceWindowField
            control={control}
            id="modify-maintenance-window"
            name="preferredMaintenanceWindow"
          />

          <TextField
            description="Leave blank to keep the current password."
            error={errors.masterUserPassword}
            id="modify-password"
            label="Reset master password"
            type="password"
            {...register("masterUserPassword")}
          />

          <DeletionProtectionField
            control={control}
            name="deletionProtection"
          />

          <ApplyImmediatelyField control={control} />

          <FieldNotes
            className="space-y-2 rounded-md border border-border p-3"
            entries={READ_ONLY_FIELDS}
            title="Fixed at create"
          />

          <CliCommandPanel
            commands={buildModifyDBInstanceCommands(identifier, values)}
          />
          <FormActions
            error={modifyInstance.error}
            errorMsg="Failed to modify the DB instance"
            isPending={modifyInstance.isPending}
            isSubmitting={isSubmitting}
            onCancel={onClose}
            pendingLabel="Saving…"
            submitLabel="Save Changes"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

// The console sends both booleans every time, so the command states both.
export function buildModifyDBInstanceCommands(
  identifier: string,
  values: DeepPartial<ModifyDBInstanceFormData>,
): CliCommand[] {
  const parts: CliCommand["parts"] = [
    { type: "bin", value: "AWS_PROFILE=spinifex aws rds modify-db-instance" },
    ...commandFlag("--db-instance-identifier", identifier),
    ...optionalFlag("--db-instance-class", values.dbInstanceClass),
    ...optionalFlag("--allocated-storage", finite(values.allocatedStorage)),
    ...optionalFlag("--db-parameter-group-name", values.dbParameterGroupName),
    ...optionalFlag(
      "--vpc-security-group-ids",
      (values.vpcSecurityGroupIds ?? []).join(" "),
    ),
    {
      type: "flag",
      value: values.deletionProtection
        ? " --deletion-protection"
        : " --no-deletion-protection",
    },
    ...optionalFlag(
      "--backup-retention-period",
      finite(values.backupRetentionPeriod),
    ),
    ...optionalFlag("--preferred-backup-window", values.preferredBackupWindow),
    ...optionalFlag(
      "--preferred-maintenance-window",
      values.preferredMaintenanceWindow,
    ),
    ...(values.masterUserPassword
      ? variableFlag("--master-user-password", "DB_PASSWORD")
      : []),
    {
      type: "flag",
      value: values.applyImmediately
        ? " --apply-immediately"
        : " --no-apply-immediately",
    },
  ]

  return [{ label: "Modify DB Instance", parts }]
}

function finite(n: number | undefined): number | undefined {
  return n !== undefined && Number.isFinite(n) ? n : undefined
}
