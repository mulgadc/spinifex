import type { SecurityGroup } from "@aws-sdk/client-ec2"
import type { DBInstance } from "@aws-sdk/client-rds"
import { type Control, Controller } from "react-hook-form"

import { type PickerNotice, PickerNoticeText } from "@/components/picker-notice"
import { SecurityGroupPicker } from "@/components/security-group-picker"
import { Field, FieldTitle } from "@/components/ui/field"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import type { ModifyDBInstanceFormData } from "@/types/rds"

export function modifyDefaults(
  instance: DBInstance,
  currentGroups: string[],
): ModifyDBInstanceFormData {
  const currentStorage = instance.AllocatedStorage ?? 0

  return {
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
  }
}

export function currentSecurityGroupIds(instance: DBInstance): string[] {
  return (
    instance.VpcSecurityGroups?.map((g) => g.VpcSecurityGroupId ?? "").filter(
      Boolean,
    ) ?? []
  )
}

// The instance's ENI is already placed, so a group from another VPC cannot be
// attached to it.
export function securityGroupsForInstance(
  groups: SecurityGroup[] | undefined,
  instance: DBInstance,
): SecurityGroup[] {
  const instanceVpc = instance.DBSubnetGroup?.VpcId

  return (groups ?? []).filter(
    (g) => instanceVpc === undefined || g.VpcId === instanceVpc,
  )
}

export function InstanceSecurityGroupsField({
  groups,
  notice,
  onChange,
  selected,
}: {
  groups: SecurityGroup[]
  notice: PickerNotice | undefined
  onChange: (next: string[]) => void
  selected: string[]
}) {
  return (
    <Field>
      <FieldTitle>VPC security groups</FieldTitle>
      {notice ? (
        <PickerNoticeText notice={notice} />
      ) : (
        <SecurityGroupPicker
          emptyText="No security group in this instance's VPC is available."
          groups={groups}
          onChange={onChange}
          selected={selected}
        />
      )}
    </Field>
  )
}

export function ApplyImmediatelyField({
  control,
}: {
  control: Control<ModifyDBInstanceFormData>
}) {
  return (
    <Field>
      <FieldTitle>Apply immediately</FieldTitle>
      <Controller
        control={control}
        name="applyImmediately"
        render={({ field }) => (
          <div className="flex items-center gap-2 text-xs">
            <Switch
              aria-label="Apply immediately"
              checked={field.value}
              id="applyImmediately"
              onCheckedChange={field.onChange}
            />
            <Label htmlFor="applyImmediately">
              Apply now instead of waiting for the maintenance window
            </Label>
          </div>
        )}
      />
    </Field>
  )
}
