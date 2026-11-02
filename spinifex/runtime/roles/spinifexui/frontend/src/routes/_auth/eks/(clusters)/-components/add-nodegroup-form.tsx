import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { Controller, useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { SelectField, selectOptions } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import {
  GpuInstanceTypeSelect,
  isGpuInstanceType,
} from "@/components/gpu-instance-type-select"
import { SwitchList } from "@/components/switch-list"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateNodegroup } from "@/mutations/eks"
import {
  ec2InstanceTypesQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import { iamRolesQueryOptions } from "@/queries/iam"
import {
  type CreateNodegroupFormValues,
  createNodegroupSchema,
  EKS_AMI_TYPES,
  EKS_CAPACITY_TYPES,
} from "@/types/eks"

// Pre-create only: the selected instance type is the sole GPU signal before a
// nodegroup exists, driving the AMI/taint note in the add-nodegroup form.
function isGpuNodegroup(
  primaryInstanceType: string | undefined,
  instanceTypes: InstanceTypeInfo[],
): boolean {
  const info = instanceTypes.find((t) => t.InstanceType === primaryInstanceType)

  return !!info && isGpuInstanceType(info)
}

function splitInstanceTypes(value: string): string[] {
  return value
    .split(",")
    .map((t) => t.trim())
    .filter(Boolean)
}

function buildCreateNodegroupCommands(
  clusterName: string,
  values: CreateNodegroupFormValues,
): CliCommand[] {
  const instanceTypes = splitInstanceTypes(values.instanceTypes)

  return [
    {
      label: "Create node group",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws eks create-nodegroup" },
        ...commandFlag("\\\n  --cluster-name", clusterName),
        ...commandFlag(
          "\\\n  --nodegroup-name",
          cliPlaceholder(values.nodegroupName, "nodegroup-name"),
        ),
        ...commandFlag(
          "\\\n  --node-role",
          cliPlaceholder(values.nodeRole, "node-role-arn"),
        ),
        ...commandFlag(
          "\\\n  --subnets",
          cliPlaceholder(values.subnetIds.join(" "), "subnet-ids"),
        ),
        ...commandFlag(
          "\\\n  --instance-types",
          cliPlaceholder(instanceTypes.join(" "), "instance-type"),
        ),
        ...commandFlag("\\\n  --ami-type", values.amiType),
        ...commandFlag("\\\n  --capacity-type", values.capacityType),
        ...commandFlag("\\\n  --disk-size", values.diskSize),
        ...commandFlag(
          "\\\n  --scaling-config",
          `minSize=${values.minSize},maxSize=${values.maxSize},desiredSize=${values.desiredSize}`,
        ),
      ],
    },
  ]
}

export function AddNodegroupForm({
  clusterName,
  vpcId,
  onClose,
}: {
  clusterName: string
  vpcId: string | undefined
  onClose: () => void
}) {
  const { data: rolesData } = useQuery(iamRolesQueryOptions)
  const { data: subnetsData } = useQuery(ec2SubnetsQueryOptions)
  const { data: instanceTypesData } = useQuery(ec2InstanceTypesQueryOptions)
  const createNodegroup = useCreateNodegroup()

  const roles = rolesData?.Roles ?? []
  const subnets = (subnetsData?.Subnets ?? []).filter((s) => s.VpcId === vpcId)
  const instanceTypes = instanceTypesData?.InstanceTypes ?? []

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
  } = useForm<CreateNodegroupFormValues>({
    resolver: zodResolver(createNodegroupSchema),
    defaultValues: {
      nodegroupName: "",
      nodeRole: "",
      subnetIds: [],
      instanceTypes: "t3.medium",
      amiType: "AL2_x86_64",
      capacityType: "ON_DEMAND",
      diskSize: 20,
      minSize: 1,
      desiredSize: 2,
      maxSize: 3,
    },
  })

  const watched = useWatch({
    control,
    name: [
      "nodegroupName",
      "nodeRole",
      "subnetIds",
      "instanceTypes",
      "amiType",
      "capacityType",
      "diskSize",
      "minSize",
      "desiredSize",
      "maxSize",
    ],
  })

  const values: CreateNodegroupFormValues = {
    nodegroupName: watched[0],
    nodeRole: watched[1],
    subnetIds: watched[2],
    instanceTypes: watched[3],
    amiType: watched[4],
    capacityType: watched[5],
    diskSize: watched[6],
    minSize: watched[7],
    desiredSize: watched[8],
    maxSize: watched[9],
  }

  const selectedIsGpu = isGpuNodegroup(values.instanceTypes, instanceTypes)

  const onSubmit = async (data: CreateNodegroupFormValues) => {
    try {
      await createNodegroup.mutateAsync({
        clusterName,
        nodegroupName: data.nodegroupName,
        nodeRole: data.nodeRole,
        subnetIds: data.subnetIds,
        instanceTypes: splitInstanceTypes(data.instanceTypes),
        amiType: data.amiType,
        capacityType: data.capacityType,
        diskSize: data.diskSize,
        minSize: data.minSize,
        desiredSize: data.desiredSize,
        maxSize: data.maxSize,
      })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel onClose={onClose} subtitle={clusterName} title="Add node group">
      <form className="space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <FormLock className="space-y-4" locked={createNodegroup.isPending}>
          <Field>
            <FieldTitle>
              <label htmlFor="ng-name">Name</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.nodegroupName}
              id="ng-name"
              placeholder="my-nodegroup"
              {...register("nodegroupName")}
            />
            <FieldError errors={[errors.nodegroupName]} />
          </Field>

          <SelectField
            control={control}
            id="ng-role"
            label="Node IAM role"
            name="nodeRole"
            options={roles.map((role) => ({
              value: role.Arn ?? "",
              label: role.RoleName ?? "",
            }))}
            placeholder="Select role"
          />

          <Field>
            <FieldTitle>Subnets</FieldTitle>
            <SwitchList
              emptyText="No subnets in the cluster VPC."
              itemLabel="Subnet"
              items={subnets.map((subnet) => ({
                id: subnet.SubnetId ?? "",
                label: subnet.SubnetId ?? "",
                detail: subnet.CidrBlock,
              }))}
              onChange={(next) => {
                setValue("subnetIds", next, { shouldValidate: true })
              }}
              selected={values.subnetIds}
            />
            <FieldError errors={[errors.subnetIds]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="ng-instance-type">Instance type</label>
            </FieldTitle>
            <Controller
              control={control}
              name="instanceTypes"
              render={({ field }) => (
                <GpuInstanceTypeSelect
                  aria-invalid={!!errors.instanceTypes}
                  id="ng-instance-type"
                  instanceTypes={instanceTypes}
                  onValueChange={field.onChange}
                  value={field.value}
                />
              )}
            />
            {selectedIsGpu && (
              <FieldDescription>
                GPU node AMI is auto-selected for this instance type. This node
                group is tainted <code>nvidia.com/gpu=present:NoSchedule</code>{" "}
                by default, so ordinary pods won&apos;t schedule onto GPU
                workers.
              </FieldDescription>
            )}
            <FieldError errors={[errors.instanceTypes]} />
          </Field>

          <div className="grid grid-cols-2 gap-3">
            <SelectField
              control={control}
              id="ng-ami"
              label="AMI type"
              name="amiType"
              options={selectOptions(EKS_AMI_TYPES)}
            />
            <SelectField
              control={control}
              id="ng-capacity"
              label="Capacity type"
              name="capacityType"
              options={selectOptions(EKS_CAPACITY_TYPES)}
            />
          </div>

          <div className="grid grid-cols-4 gap-3">
            <Field>
              <FieldTitle>
                <label htmlFor="ng-disk">Disk (GB)</label>
              </FieldTitle>
              <Input
                id="ng-disk"
                type="number"
                {...register("diskSize", { valueAsNumber: true })}
              />
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="ng-min">Min</label>
              </FieldTitle>
              <Input
                id="ng-min"
                type="number"
                {...register("minSize", { valueAsNumber: true })}
              />
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="ng-desired">Desired</label>
              </FieldTitle>
              <Input
                id="ng-desired"
                type="number"
                {...register("desiredSize", { valueAsNumber: true })}
              />
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="ng-max">Max</label>
              </FieldTitle>
              <Input
                id="ng-max"
                type="number"
                {...register("maxSize", { valueAsNumber: true })}
              />
            </Field>
          </div>
          <FieldError errors={[errors.desiredSize]} />
        </FormLock>

        <CliCommandPanel
          commands={buildCreateNodegroupCommands(clusterName, values)}
        />

        <FormActions
          error={createNodegroup.error}
          errorMsg="Failed to create the node group"
          isPending={createNodegroup.isPending}
          isSubmitting={isSubmitting}
          onCancel={onClose}
          pendingLabel="Creating…"
          submitLabel="Create"
        />
      </form>
    </FormPanel>
  )
}
