import type { SecurityGroup } from "@aws-sdk/client-ec2"

import { SwitchList } from "@/components/switch-list"
import { securityGroupLabel } from "@/lib/utils"

interface SecurityGroupPickerProps {
  emptyText: string
  groups: SecurityGroup[]
  onChange: (next: string[]) => void
  selected: string[]
}

export function securityGroupsForVpc(
  groups: SecurityGroup[],
  vpcId: string | undefined,
): SecurityGroup[] {
  return vpcId ? groups.filter((group) => group.VpcId === vpcId) : []
}

export function securityGroupIdsForVpc(
  groups: SecurityGroup[],
  vpcId: string | undefined,
  selected: string[],
): string[] {
  const available = new Set(
    securityGroupsForVpc(groups, vpcId).map((group) => group.GroupId),
  )

  return selected.filter((id) => available.has(id))
}

export function defaultSecurityGroupIdForVpc(
  groups: SecurityGroup[],
  vpcId: string | undefined,
): string | undefined {
  return securityGroupsForVpc(groups, vpcId).find(
    (group) => group.GroupName === "default",
  )?.GroupId
}

// Multi-select of security groups for an ENI. The caller has
// narrowed the list to the placement VPC, so anything rendered is attachable.
export function SecurityGroupPicker({
  emptyText,
  groups,
  onChange,
  selected,
}: SecurityGroupPickerProps) {
  return (
    <SwitchList
      emptyText={emptyText}
      itemLabel="Security group"
      items={groups.map((group) => ({
        id: group.GroupId ?? "",
        label: securityGroupLabel(group),
        detail: group.Description,
      }))}
      onChange={onChange}
      selected={selected}
    />
  )
}
