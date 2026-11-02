import type { SecurityGroup, Subnet } from "@aws-sdk/client-ec2"

import { PanelSection } from "@/components/detail-panel"
import { SecurityGroupPicker } from "@/components/security-group-picker"
import { SwitchList } from "@/components/switch-list"
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
import { arnResourceId, getNameTag } from "@/lib/utils"

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

// awsvpc tasks need a subnet for their ENI; the schema cannot know the
// network mode, which comes from the chosen task definition.
export const AWSVPC_SUBNET_REQUIRED =
  "Select at least one subnet for awsvpc networking"

interface AwsvpcNetworkFieldsProps {
  assignPublicIp: boolean
  onAssignPublicIpChange: (next: boolean) => void
  onSecurityGroupsChange: (next: string[]) => void
  onSubnetsChange: (next: string[]) => void
  securityGroups: SecurityGroup[]
  selectedSecurityGroups: string[]
  selectedSubnets: string[]
  subnets: Subnet[]
  subnetsError?: { message?: string }
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
  subnetsError,
}: AwsvpcNetworkFieldsProps) {
  return (
    <PanelSection title="Network">
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          This task definition uses awsvpc networking. Select at least one
          subnet.
        </p>
        <Field>
          <FieldTitle>Subnets</FieldTitle>
          <SwitchList
            emptyText="No subnets available."
            itemLabel="Subnet"
            items={subnets.map((subnet) => ({
              id: subnet.SubnetId ?? "",
              label: subnet.SubnetId ?? "",
              detail: [
                getNameTag(subnet.Tags),
                subnet.AvailabilityZone,
                subnet.CidrBlock,
              ]
                .filter(Boolean)
                .join(" · "),
            }))}
            onChange={onSubnetsChange}
            selected={selectedSubnets}
          />
          <FieldError errors={[subnetsError]} />
        </Field>
        <Field>
          <FieldTitle>Security groups</FieldTitle>
          <SecurityGroupPicker
            emptyText="No security groups available."
            groups={securityGroups}
            onChange={onSecurityGroupsChange}
            selected={selectedSecurityGroups}
          />
        </Field>
        <div className="flex items-center gap-2">
          <Switch
            checked={assignPublicIp}
            id="assignPublicIp"
            onCheckedChange={onAssignPublicIpChange}
          />
          <Label htmlFor="assignPublicIp">Assign public IP</Label>
        </div>
      </div>
    </PanelSection>
  )
}
