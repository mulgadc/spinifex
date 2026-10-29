import type { SecurityGroup, Subnet } from "@aws-sdk/client-ec2"

import { SecurityGroupCheckboxes } from "@/components/security-group-checkboxes"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { arnResourceId } from "@/lib/utils"

interface TaskDefinitionFieldProps {
  arns: string[]
  error?: { message?: string }
  onChange: (value: string | null) => void
  value: string
}

// Lists task definitions by family:revision, the form RunTask and
// CreateService accept in place of the full ARN.
export function TaskDefinitionField({
  arns,
  error,
  onChange,
  value,
}: TaskDefinitionFieldProps) {
  return (
    <Field>
      <FieldTitle>
        <label htmlFor="taskDefinition">Task definition</label>
      </FieldTitle>
      <Select onValueChange={onChange} value={value}>
        <SelectTrigger className="w-full" id="taskDefinition">
          <SelectValue placeholder="Select a task definition" />
        </SelectTrigger>
        <SelectContent>
          {arns.map((arn) => {
            const id = arnResourceId(arn)

            return (
              <SelectItem key={arn} value={id}>
                {id}
              </SelectItem>
            )
          })}
        </SelectContent>
      </Select>
      <FieldError errors={[error]} />
    </Field>
  )
}

interface AwsvpcNetworkFieldsProps {
  assignPublicIp: boolean
  onAssignPublicIpChange: (next: boolean) => void
  onSecurityGroupsChange: (next: string[]) => void
  onSubnetsChange: (next: string[]) => void
  securityGroups: SecurityGroup[]
  selectedSecurityGroups: string[]
  selectedSubnets: string[]
  subnets: Subnet[]
}

export function AwsvpcNetworkFields({
  assignPublicIp,
  onAssignPublicIpChange,
  onSecurityGroupsChange,
  onSubnetsChange,
  securityGroups,
  selectedSecurityGroups,
  selectedSubnets,
  subnets,
}: AwsvpcNetworkFieldsProps) {
  const selectedSubnetSet = new Set(selectedSubnets)

  const toggleSubnet = (id: string) => {
    onSubnetsChange(
      selectedSubnetSet.has(id)
        ? selectedSubnets.filter((x) => x !== id)
        : [...selectedSubnets, id],
    )
  }

  return (
    <div className="space-y-4 rounded-lg border bg-card p-4">
      <p className="text-sm text-muted-foreground">
        This task definition uses awsvpc networking. Select at least one subnet.
      </p>
      <div>
        <span className="text-sm font-medium">Subnets</span>
        <div className="mt-2 space-y-1">
          {subnets.map((s) => (
            <label className="flex items-center gap-2 text-xs" key={s.SubnetId}>
              <input
                checked={selectedSubnetSet.has(s.SubnetId ?? "")}
                onChange={() => {
                  toggleSubnet(s.SubnetId ?? "")
                }}
                type="checkbox"
              />
              <span className="font-mono">
                {s.SubnetId} ({s.CidrBlock})
              </span>
            </label>
          ))}
        </div>
      </div>
      <div>
        <span className="text-sm font-medium">Security groups</span>
        <div className="mt-2">
          <SecurityGroupCheckboxes
            emptyText="No security groups available."
            groups={securityGroups}
            onChange={onSecurityGroupsChange}
            selected={selectedSecurityGroups}
          />
        </div>
      </div>
      <div className="flex items-center gap-2">
        <Switch
          checked={assignPublicIp}
          id="assignPublicIp"
          onCheckedChange={onAssignPublicIpChange}
        />
        <Label htmlFor="assignPublicIp">Assign public IP</Label>
      </div>
    </div>
  )
}
