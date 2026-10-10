import type { SecurityGroup } from "@aws-sdk/client-ec2"

import { securityGroupLabel } from "@/lib/utils"

interface SecurityGroupCheckboxesProps {
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

// The multi-select every RDS form uses for the instance's ENI. The caller has
// narrowed the list to the placement VPC, so anything rendered is attachable.
export function SecurityGroupCheckboxes({
  emptyText,
  groups,
  onChange,
  selected,
}: SecurityGroupCheckboxesProps) {
  if (groups.length === 0) {
    return <p className="text-xs text-muted-foreground">{emptyText}</p>
  }

  const selectedIds = new Set(selected)

  const toggle = (groupId: string) => {
    onChange(
      selected.includes(groupId)
        ? selected.filter((id) => id !== groupId)
        : [...selected, groupId],
    )
  }

  return (
    <div className="space-y-1">
      {groups.map((group) => (
        <label className="flex items-center gap-2 text-xs" key={group.GroupId}>
          <input
            aria-label={`Security group ${securityGroupLabel(group)}`}
            checked={selectedIds.has(group.GroupId ?? "")}
            onChange={() => {
              toggle(group.GroupId ?? "")
            }}
            type="checkbox"
          />
          <span className="font-mono">{securityGroupLabel(group)}</span>
        </label>
      ))}
    </div>
  )
}
