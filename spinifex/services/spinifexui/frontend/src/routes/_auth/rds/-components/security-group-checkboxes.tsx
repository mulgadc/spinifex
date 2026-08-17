import type { SecurityGroup } from "@aws-sdk/client-ec2"

import { securityGroupLabel } from "@/lib/utils"

interface SecurityGroupCheckboxesProps {
  emptyText: string
  groups: SecurityGroup[]
  onChange: (next: string[]) => void
  selected: string[]
}

// The multi-select every RDS form uses for the instance's ENI. The caller has
// already narrowed the list to the VPC the subnet group pins, so anything
// rendered here is attachable.
export function SecurityGroupCheckboxes({
  emptyText,
  groups,
  onChange,
  selected,
}: SecurityGroupCheckboxesProps) {
  if (groups.length === 0) {
    return <p className="text-xs text-muted-foreground">{emptyText}</p>
  }

  const toggle = (groupId: string) =>
    onChange(
      selected.includes(groupId)
        ? selected.filter((id) => id !== groupId)
        : [...selected, groupId],
    )

  return (
    <div className="space-y-1">
      {groups.map((group) => (
        <label className="flex items-center gap-2 text-xs" key={group.GroupId}>
          <input
            aria-label={`Security group ${securityGroupLabel(group)}`}
            checked={selected.includes(group.GroupId ?? "")}
            onChange={() => toggle(group.GroupId ?? "")}
            type="checkbox"
          />
          <span className="font-mono">{securityGroupLabel(group)}</span>
        </label>
      ))}
    </div>
  )
}
