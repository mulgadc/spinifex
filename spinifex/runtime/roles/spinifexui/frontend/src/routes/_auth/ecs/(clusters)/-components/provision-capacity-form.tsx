import { useQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { GpuInstanceTypeSelect } from "@/components/gpu-instance-type-select"
import { SystemImageRequired } from "@/components/system-image-required"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import type { ProvisionCapacityRequest } from "@/lib/ecs-provision"
import { isEcsSystemImage } from "@/lib/system-managed"
import { securityGroupLabel } from "@/lib/utils"
import { useProvisionCapacity } from "@/mutations/ecs"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"

interface ProvisionCapacityFormProps {
  clusterName: string
  onClose: () => void
}

interface PickerOption {
  value: string
  label: string
}

function PickerField({
  id,
  label,
  options,
  placeholder,
  value,
  onValueChange,
}: {
  id: string
  label: string
  options: PickerOption[]
  placeholder: string
  value: string
  onValueChange: (value: string) => void
}) {
  return (
    <Field>
      <FieldTitle>
        <label htmlFor={id}>{label}</label>
      </FieldTitle>
      <Select
        items={options}
        onValueChange={(next: string | null) => {
          onValueChange(next ?? "")
        }}
        value={value}
      >
        <SelectTrigger className="w-full" id={id}>
          <SelectValue placeholder={placeholder} />
        </SelectTrigger>
        <SelectContent>
          {options.map((option) => (
            <SelectItem key={option.value} value={option.value}>
              {option.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  )
}

// ProvisionCapacity is a Spinifex gateway action the AWS CLI has no command
// for, so the panel shows the JSON-1.1 body the console signs and sends.
function buildProvisionCommands(req: ProvisionCapacityRequest): CliCommand[] {
  return [
    {
      label: "Provision capacity",
      parts: [
        {
          type: "comment",
          value:
            "# No AWS CLI command: POST to the ECS endpoint with X-Amz-Target AmazonEC2ContainerServiceV20141113.ProvisionCapacity\n",
        },
        { type: "value", value: JSON.stringify(req, null, 2) },
      ],
    },
  ]
}

export function ProvisionCapacityForm({
  clusterName,
  onClose,
}: ProvisionCapacityFormProps) {
  const provision = useProvisionCapacity()
  const { data: subnetsData } = useQuery(ec2SubnetsQueryOptions)
  const { data: sgData } = useQuery(ec2SecurityGroupsQueryOptions)
  const { data: keyPairsData } = useQuery(ec2KeyPairsQueryOptions)
  const { data: instanceTypesData } = useQuery(ec2InstanceTypesQueryOptions)
  const { data: imagesData } = useQuery(ec2ImagesQueryOptions)

  // Container instances boot the ECS node image; the cluster itself does not.
  const missingImage =
    imagesData !== undefined &&
    !(imagesData.Images ?? []).some(isEcsSystemImage)

  const subnets = subnetsData?.Subnets ?? []
  const securityGroups = sgData?.SecurityGroups ?? []
  const keyPairs = keyPairsData?.KeyPairs ?? []
  const instanceTypes = instanceTypesData?.InstanceTypes ?? []

  const [instanceType, setInstanceType] = useState("t3.small")
  const [count, setCount] = useState(1)
  const [subnetId, setSubnetId] = useState("")
  const [securityGroupId, setSecurityGroupId] = useState("")
  const [keyName, setKeyName] = useState("")

  const request: ProvisionCapacityRequest = {
    Cluster: clusterName,
    InstanceType: instanceType || undefined,
    Count: count,
    SubnetID: subnetId,
    SecurityGroupID: securityGroupId,
    KeyName: keyName || undefined,
  }

  async function handleProvision() {
    try {
      await provision.mutateAsync(request)
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={clusterName}
      title="Provision EC2 capacity"
    >
      <form
        className="space-y-6"
        onSubmit={(event) => {
          event.preventDefault()
          void handleProvision()
        }}
      >
        <p className="text-sm text-muted-foreground">
          Launch container instances into cluster &ldquo;{clusterName}&rdquo;.
          They register automatically once booted.
        </p>

        <FormLock className="space-y-4" locked={provision.isPending}>
          {missingImage ? (
            <SystemImageRequired
              description="Container instances boot a Spinifex-managed node image (containerd + ecs-agent). Import it before provisioning capacity."
              importCommand="spx admin images import --name spinifex-ecs-node"
              title="ECS system image not found"
            />
          ) : (
            <>
              <Field>
                <FieldTitle>
                  <label htmlFor="provision-instance-type">Instance type</label>
                </FieldTitle>
                <GpuInstanceTypeSelect
                  id="provision-instance-type"
                  instanceTypes={instanceTypes}
                  onValueChange={setInstanceType}
                  value={instanceType}
                />
              </Field>

              <Field>
                <FieldTitle>
                  <label htmlFor="provision-count">Count</label>
                </FieldTitle>
                <Input
                  id="provision-count"
                  max={10}
                  min={1}
                  onChange={(e) => {
                    setCount(Number(e.target.value))
                  }}
                  type="number"
                  value={count}
                />
              </Field>

              <PickerField
                id="provision-subnet"
                label="Subnet"
                onValueChange={setSubnetId}
                options={subnets.map((s) => ({
                  value: s.SubnetId ?? "",
                  label: `${s.SubnetId ?? ""} (${s.CidrBlock ?? ""}${s.AvailabilityZone ? `, ${s.AvailabilityZone}` : ""})`,
                }))}
                placeholder="Select a subnet"
                value={subnetId}
              />

              <PickerField
                id="provision-sg"
                label="Security group"
                onValueChange={setSecurityGroupId}
                options={securityGroups.map((sg) => ({
                  value: sg.GroupId ?? "",
                  label: securityGroupLabel(sg),
                }))}
                placeholder="Select a security group"
                value={securityGroupId}
              />

              <PickerField
                id="provision-key"
                label="Key pair"
                onValueChange={setKeyName}
                options={[
                  { value: "", label: "None" },
                  ...keyPairs.map((kp) => ({
                    value: kp.KeyName ?? "",
                    label: kp.KeyName ?? "",
                  })),
                ]}
                placeholder="None"
                value={keyName}
              />
            </>
          )}
        </FormLock>

        {!missingImage && (
          <CliCommandPanel commands={buildProvisionCommands(request)} />
        )}

        <FormActions
          error={provision.error}
          errorMsg="Failed to provision capacity."
          isPending={provision.isPending}
          isSubmitting={false}
          onCancel={onClose}
          pendingLabel="Provisioning…"
          submitDisabled={
            missingImage || subnetId === "" || securityGroupId === ""
          }
          submitLabel="Provision"
        />
      </form>
    </FormPanel>
  )
}
