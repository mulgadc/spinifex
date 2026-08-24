import type { DBInstance } from "@aws-sdk/client-rds"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { Controller, useForm, useWatch } from "react-hook-form"

import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
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

import { PickerNoticeText, pickerNotice } from "../../-components/picker-notice"
import {
  DeletionProtectionField,
  parameterGroupsForEngine,
  RdsSelectField,
} from "../../-components/rds-form-fields"
import { SecurityGroupCheckboxes } from "../../-components/security-group-checkboxes"

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

interface ModifyDBInstanceDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  instance: DBInstance
}

export function ModifyDBInstanceDialog({
  open,
  onOpenChange,
  instance,
}: ModifyDBInstanceDialogProps) {
  const modifyInstance = useModifyDBInstance()
  const parameterGroupsQuery = useQuery(rdsParameterGroupsQueryOptions)
  const securityGroupsQuery = useQuery(ec2SecurityGroupsQueryOptions)
  const engineVersionsQuery = useQuery(rdsEngineVersionsQueryOptions)
  const orderableQuery = useQuery(
    rdsOrderableOptionsQueryOptions(instance.Engine ?? ""),
  )

  const identifier = instance.DBInstanceIdentifier ?? ""
  const currentStorage = instance.AllocatedStorage ?? 0
  const currentGroups =
    instance.VpcSecurityGroups?.map((g) => g.VpcSecurityGroupId ?? "").filter(
      Boolean,
    ) ?? []

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
  } = useForm<ModifyDBInstanceFormData>({
    resolver: zodResolver(modifyDBInstanceSchema),
    defaultValues: {
      currentAllocatedStorage: currentStorage,
      dbInstanceClass: instance.DBInstanceClass ?? "",
      allocatedStorage: currentStorage,
      dbParameterGroupName:
        instance.DBParameterGroups?.[0]?.DBParameterGroupName ?? "",
      vpcSecurityGroupIds: currentGroups,
      deletionProtection: instance.DeletionProtection ?? false,
      backupRetentionPeriod: instance.BackupRetentionPeriod ?? 0,
      preferredBackupWindow: instance.PreferredBackupWindow ?? "",
      preferredMaintenanceWindow: instance.PreferredMaintenanceWindow ?? "",
      masterUserPassword: "",
      applyImmediately: false,
    },
  })

  const values = useWatch({ control })
  const selectedClass = values.dbInstanceClass ?? ""
  const selectedStorage = values.allocatedStorage ?? currentStorage
  const selectedGroups = values.vpcSecurityGroupIds ?? []

  const instanceClasses = [
    ...new Set(
      (orderableQuery.data?.OrderableDBInstanceOptions ?? [])
        .map((o) => o.DBInstanceClass ?? "")
        .filter(Boolean),
    ),
  ]
  const parameterGroups = parameterGroupsForEngine(
    parameterGroupsQuery.data?.DBParameterGroups ?? [],
    engineVersionsQuery.data?.DBEngineVersions ?? [],
    instance.Engine ?? "",
  )
  // The instance's ENI is already placed, so a group from another VPC cannot be
  // attached to it.
  const instanceVpc = instance.DBSubnetGroup?.VpcId
  const securityGroups = (
    securityGroupsQuery.data?.SecurityGroups ?? []
  ).filter((g) => instanceVpc === undefined || g.VpcId === instanceVpc)

  const classChanged = selectedClass !== instance.DBInstanceClass
  const storageChanged = selectedStorage !== currentStorage

  const classNotice = pickerNotice(
    orderableQuery,
    instanceClasses.length === 0,
    {
      loading: "Loading the instance classes…",
      failed: "Could not read the instance classes",
      empty: `No instance class this cluster's nodes can run is available for ${instance.Engine}.`,
    },
  )
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
    await modifyInstance.mutateAsync({
      ...data,
      dbInstanceIdentifier: identifier,
    })
    onOpenChange(false)
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent className="max-h-[85vh] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>Modify {identifier}</AlertDialogTitle>
          <AlertDialogDescription>
            Changes apply in the maintenance window unless you apply them
            immediately.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <form className="space-y-4" onSubmit={handleSubmit(onSubmit)}>
          <RdsSelectField
            control={control}
            id="modify-class"
            label="DB instance class"
            name="dbInstanceClass"
            notice={classNotice}
            options={instanceClasses.map((c) => ({ value: c, label: c }))}
            placeholder="Select an instance class"
            warning={
              classChanged &&
              "Changing the class replaces the VM. The database is unavailable while it does."
            }
          />

          <Field>
            <FieldTitle>
              <label htmlFor="modify-storage">Allocated storage (GiB)</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.allocatedStorage}
              id="modify-storage"
              min={currentStorage}
              type="number"
              {...register("allocatedStorage", { valueAsNumber: true })}
            />
            <FieldDescription>
              Currently {currentStorage} GiB. Storage grows only.
            </FieldDescription>
            {storageChanged && (
              <p className="text-xs text-tactical-amber">
                Growing storage stops and starts the instance to extend the
                filesystem.
              </p>
            )}
            <FieldError errors={[errors.allocatedStorage]} />
          </Field>

          <RdsSelectField
            control={control}
            id="modify-parameter-group"
            label="DB parameter group"
            name="dbParameterGroupName"
            notice={parameterGroupNotice}
            options={parameterGroups.map((g) => ({
              value: g.DBParameterGroupName ?? "",
              label: g.DBParameterGroupName ?? "",
            }))}
            placeholder="Engine default group"
          />

          <Field>
            <FieldTitle>VPC security groups</FieldTitle>
            {securityGroupNotice ? (
              <PickerNoticeText notice={securityGroupNotice} />
            ) : (
              <SecurityGroupCheckboxes
                emptyText="No security group in this instance's VPC is available."
                groups={securityGroups}
                onChange={setSecurityGroups}
                selected={selectedGroups}
              />
            )}
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="modify-retention">Backup retention (days)</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.backupRetentionPeriod}
              id="modify-retention"
              max={MAX_BACKUP_RETENTION_DAYS}
              min={0}
              type="number"
              {...register("backupRetentionPeriod", { valueAsNumber: true })}
            />
            <FieldError errors={[errors.backupRetentionPeriod]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="modify-backup-window">
                Preferred backup window
              </label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.preferredBackupWindow}
              id="modify-backup-window"
              placeholder="hh24:mi-hh24:mi in UTC"
              {...register("preferredBackupWindow")}
            />
            <FieldError errors={[errors.preferredBackupWindow]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="modify-maintenance-window">
                Preferred maintenance window
              </label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.preferredMaintenanceWindow}
              id="modify-maintenance-window"
              placeholder="ddd:hh24:mi-ddd:hh24:mi in UTC"
              {...register("preferredMaintenanceWindow")}
            />
            <FieldError errors={[errors.preferredMaintenanceWindow]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="modify-password">Reset master password</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.masterUserPassword}
              id="modify-password"
              type="password"
              {...register("masterUserPassword")}
            />
            <FieldDescription>
              Leave blank to keep the current password.
            </FieldDescription>
            <FieldError errors={[errors.masterUserPassword]} />
          </Field>

          <DeletionProtectionField
            control={control}
            name="deletionProtection"
          />

          <Field>
            <FieldTitle>Apply immediately</FieldTitle>
            <Controller
              control={control}
              name="applyImmediately"
              render={({ field }) => (
                <label className="flex items-center gap-2 text-xs">
                  <input
                    aria-label="Apply immediately"
                    checked={field.value}
                    onChange={(e) => {
                      field.onChange(e.target.checked)
                    }}
                    type="checkbox"
                  />
                  <span>
                    Apply now instead of waiting for the maintenance window
                  </span>
                </label>
              )}
            />
          </Field>

          <div className="space-y-2 rounded-md border border-border p-3">
            <h3 className="text-xs font-medium">Fixed at create</h3>
            {READ_ONLY_FIELDS.map((entry) => (
              <p className="text-xs text-muted-foreground" key={entry.label}>
                <span className="font-medium">{entry.label}</span> —{" "}
                {entry.note}
              </p>
            ))}
          </div>

          {modifyInstance.error && (
            <ErrorBanner
              error={modifyInstance.error}
              msg="Failed to modify the DB instance"
            />
          )}

          <div className="flex justify-end">
            <FormActions
              isPending={modifyInstance.isPending}
              isSubmitting={isSubmitting}
              onCancel={() => {
                onOpenChange(false)
              }}
              pendingLabel="Saving…"
              submitLabel="Save Changes"
            />
          </div>
        </form>
      </AlertDialogContent>
    </AlertDialog>
  )
}
