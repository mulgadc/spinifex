import { zodResolver } from "@hookform/resolvers/zod"
import { Controller, useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  useAuthorizeSecurityGroupEgress,
  useAuthorizeSecurityGroupIngress,
} from "@/mutations/ec2"
import {
  type SecurityGroupRuleFormData,
  securityGroupRuleSchema,
} from "@/types/ec2"

type Direction = "ingress" | "egress"

function buildRuleCommands(
  direction: Direction,
  groupId: string,
  rule: SecurityGroupRuleFormData,
): CliCommand[] {
  const permission = [
    `IpProtocol=${rule.ipProtocol}`,
    `FromPort=${rule.fromPort}`,
    `ToPort=${rule.toPort}`,
    `IpRanges=[{CidrIp=${cliPlaceholder(rule.cidrIp, "CidrIp")}}]`,
  ].join(",")

  return [
    {
      label: direction === "ingress" ? "Authorize Ingress" : "Authorize Egress",
      parts: [
        {
          type: "bin",
          value: `AWS_PROFILE=spinifex aws ec2 authorize-security-group-${direction}`,
        },
        ...commandFlag("\\\n  --group-id", groupId),
        ...commandFlag("\\\n  --ip-permissions", permission),
      ],
    },
  ]
}

interface AddSecurityGroupRuleFormProps {
  direction: Direction
  groupId: string
  onClose: () => void
}

export function AddSecurityGroupRuleForm({
  direction,
  groupId,
  onClose,
}: AddSecurityGroupRuleFormProps) {
  const authorizeIngress = useAuthorizeSecurityGroupIngress()
  const authorizeEgress = useAuthorizeSecurityGroupEgress()
  const mutation = direction === "ingress" ? authorizeIngress : authorizeEgress

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<SecurityGroupRuleFormData>({
    resolver: zodResolver(securityGroupRuleSchema),
    defaultValues: {
      ipProtocol: "tcp",
      fromPort: 0,
      toPort: 0,
      cidrIp: "0.0.0.0/0",
    },
  })

  const selectedProtocol = useWatch({ control, name: "ipProtocol" })
  const portsDisabled = selectedProtocol === "-1" || selectedProtocol === "icmp"
  const fromPort = useWatch({ control, name: "fromPort" })
  const toPort = useWatch({ control, name: "toPort" })
  const cidrIp = useWatch({ control, name: "cidrIp" })

  const onSubmit = async (data: SecurityGroupRuleFormData) => {
    try {
      await mutation.mutateAsync({
        groupId,
        ipProtocol: data.ipProtocol,
        fromPort: portsDisabled ? -1 : data.fromPort,
        toPort: portsDisabled ? -1 : data.toPort,
        cidrIp: data.cidrIp,
      })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={groupId}
      title={`Add ${direction === "ingress" ? "inbound" : "outbound"} rule`}
    >
      <form onSubmit={handleSubmit(onSubmit)}>
        <FormLock className="space-y-6" locked={mutation.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor={`${direction}-ipProtocol`}>Protocol</label>
            </FieldTitle>
            <Controller
              control={control}
              name="ipProtocol"
              render={({ field }) => (
                <Select onValueChange={field.onChange} value={field.value}>
                  <SelectTrigger
                    className="w-full"
                    id={`${direction}-ipProtocol`}
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="tcp">TCP</SelectItem>
                    <SelectItem value="udp">UDP</SelectItem>
                    <SelectItem value="icmp">ICMP</SelectItem>
                    <SelectItem value="-1">All traffic (-1)</SelectItem>
                  </SelectContent>
                </Select>
              )}
            />
            <FieldError errors={[errors.ipProtocol]} />
          </Field>

          <div className="grid grid-cols-2 gap-4">
            <Field>
              <FieldTitle>
                <label htmlFor={`${direction}-fromPort`}>From port</label>
              </FieldTitle>
              <Input
                aria-invalid={!!errors.fromPort}
                disabled={portsDisabled}
                id={`${direction}-fromPort`}
                type="number"
                {...register("fromPort", { valueAsNumber: true })}
              />
              <FieldError errors={[errors.fromPort]} />
            </Field>

            <Field>
              <FieldTitle>
                <label htmlFor={`${direction}-toPort`}>To port</label>
              </FieldTitle>
              <Input
                aria-invalid={!!errors.toPort}
                disabled={portsDisabled}
                id={`${direction}-toPort`}
                type="number"
                {...register("toPort", { valueAsNumber: true })}
              />
              <FieldError errors={[errors.toPort]} />
            </Field>
          </div>

          <Field>
            <FieldTitle>
              <label htmlFor={`${direction}-cidrIp`}>
                {direction === "ingress" ? "Source" : "Destination"} CIDR
              </label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.cidrIp}
              id={`${direction}-cidrIp`}
              placeholder="0.0.0.0/0"
              {...register("cidrIp")}
            />
            <FieldError errors={[errors.cidrIp]} />
          </Field>
          <CliCommandPanel
            commands={buildRuleCommands(direction, groupId, {
              ipProtocol: selectedProtocol,
              fromPort: portsDisabled ? -1 : fromPort,
              toPort: portsDisabled ? -1 : toPort,
              cidrIp,
            })}
          />

          <FormActions
            error={mutation.error}
            errorMsg="Failed to add rule"
            isPending={mutation.isPending}
            isSubmitting={isSubmitting}
            onCancel={onClose}
            pendingLabel="Adding…"
            submitLabel="Add rule"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}
