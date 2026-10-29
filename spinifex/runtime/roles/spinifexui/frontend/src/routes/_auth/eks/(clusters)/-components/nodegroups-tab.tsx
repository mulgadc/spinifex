import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Plus, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import {
  GpuInstanceTypeSelect,
  isGpuInstanceType,
} from "@/components/gpu-instance-type-select"
import { StateBadge } from "@/components/state-badge"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  useCreateNodegroup,
  useDeleteNodegroup,
  useScaleNodegroup,
} from "@/mutations/eks"
import {
  ec2InstanceTypesQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import {
  eksNodegroupQueryOptions,
  eksNodegroupsQueryOptions,
} from "@/queries/eks"
import { iamRolesQueryOptions } from "@/queries/iam"
import {
  type CreateNodegroupFormValues,
  createNodegroupSchema,
  EKS_AMI_TYPES,
  EKS_CAPACITY_TYPES,
} from "@/types/eks"

// GPU AMI types per the AWS EKS AMITypes enum (AL2_x86_64_GPU,
// BOTTLEROCKET_x86_64_NVIDIA, BOTTLEROCKET_ARM_64_NVIDIA). DescribeNodegroup
// forces amiType to the GPU variant whenever the nodegroup is GPU-enabled, so
// an already-created nodegroup's GPU badge reads that response field instead
// of re-deriving it from instance types.
function isGpuAmiType(amiType: string | undefined): boolean {
  return !!amiType && (amiType.endsWith("_GPU") || amiType.endsWith("_NVIDIA"))
}

// Pre-create only: the selected instance type is the sole GPU signal before a
// nodegroup exists, driving the AMI/taint note in the add-nodegroup form.
function isGpuNodegroup(
  primaryInstanceType: string | undefined,
  instanceTypes: InstanceTypeInfo[],
): boolean {
  const info = instanceTypes.find((t) => t.InstanceType === primaryInstanceType)

  return !!info && isGpuInstanceType(info)
}

function ScaleNodegroupDialog({
  clusterName,
  nodegroupName,
  current,
  onOpenChange,
}: {
  clusterName: string
  nodegroupName: string
  current: { minSize?: number; desiredSize?: number; maxSize?: number }
  onOpenChange: (open: boolean) => void
}) {
  const scaleNodegroup = useScaleNodegroup()
  const [minSize, setMinSize] = useState(current.minSize ?? 0)
  const [desiredSize, setDesiredSize] = useState(current.desiredSize ?? 0)
  const [maxSize, setMaxSize] = useState(current.maxSize ?? 0)
  const [error, setError] = useState<string | undefined>()

  const handleConfirm = () => {
    if (!(minSize <= desiredSize && desiredSize <= maxSize)) {
      setError("Sizes must satisfy min ≤ desired ≤ max")

      return
    }

    scaleNodegroup.mutate(
      { clusterName, nodegroupName, minSize, desiredSize, maxSize },
      {
        onSuccess: () => {
          onOpenChange(false)
        },
      },
    )
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Scale {nodegroupName}</AlertDialogTitle>
        </AlertDialogHeader>

        <div className="grid grid-cols-3 gap-3">
          <Field>
            <FieldTitle>
              <label htmlFor="scale-min">Min</label>
            </FieldTitle>
            <Input
              id="scale-min"
              onChange={(e) => {
                setMinSize(Number(e.target.value))
              }}
              type="number"
              value={minSize}
            />
          </Field>
          <Field>
            <FieldTitle>
              <label htmlFor="scale-desired">Desired</label>
            </FieldTitle>
            <Input
              id="scale-desired"
              onChange={(e) => {
                setDesiredSize(Number(e.target.value))
              }}
              type="number"
              value={desiredSize}
            />
          </Field>
          <Field>
            <FieldTitle>
              <label htmlFor="scale-max">Max</label>
            </FieldTitle>
            <Input
              id="scale-max"
              onChange={(e) => {
                setMaxSize(Number(e.target.value))
              }}
              type="number"
              value={maxSize}
            />
          </Field>
        </div>
        {error && <p className="mt-2 text-xs text-destructive">{error}</p>}

        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={scaleNodegroup.isPending}
            onClick={(e) => {
              e.preventDefault()
              handleConfirm()
            }}
          >
            {scaleNodegroup.isPending ? "Scaling…" : "Scale"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

function NodegroupRow({
  clusterName,
  clusterVersion,
  nodegroupName,
  onDelete,
}: {
  clusterName: string
  clusterVersion: string | undefined
  nodegroupName: string
  onDelete: (name: string) => void
}) {
  const { data } = useQuery(
    eksNodegroupQueryOptions(clusterName, nodegroupName),
  )

  const ng = data?.nodegroup
  const scaling = ng?.scalingConfig
  const [showScale, setShowScale] = useState(false)

  const updateAvailable =
    !!clusterVersion && !!ng?.version && ng.version !== clusterVersion

  const isGpu = isGpuAmiType(ng?.amiType)

  return (
    <div className="rounded-md border px-3 py-2">
      <div className="flex items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate text-sm font-medium">{nodegroupName}</span>
          {isGpu && <Badge variant="secondary">GPU</Badge>}
        </div>
        <div className="flex items-center gap-1">
          <Button
            aria-label="Scale node group"
            onClick={() => {
              setShowScale(true)
            }}
            size="icon-sm"
            variant="ghost"
          >
            <SlidersHorizontal />
          </Button>
          <Button
            aria-label="Delete node group"
            onClick={() => {
              onDelete(nodegroupName)
            }}
            size="icon-sm"
            variant="ghost"
          >
            <Trash2 />
          </Button>
        </div>
      </div>
      <PanelFields>
        <PanelField label="Status">
          {ng?.status && <StateBadge state={ng.status} />}
        </PanelField>
        <PanelField label="Kubernetes version">
          {updateAvailable
            ? `${ng?.version} (cluster on ${clusterVersion})`
            : ng?.version}
        </PanelField>
        <PanelField label="Instance types" mono>
          {ng?.instanceTypes?.join(", ")}
        </PanelField>
        <PanelField label="AMI type" mono>
          {ng?.amiType}
        </PanelField>
        <PanelField label="Capacity type">{ng?.capacityType}</PanelField>
        <PanelField label="Scaling (min / desired / max)">
          {scaling
            ? `${scaling.minSize} / ${scaling.desiredSize} / ${scaling.maxSize}`
            : undefined}
        </PanelField>
      </PanelFields>

      {showScale && (
        <ScaleNodegroupDialog
          clusterName={clusterName}
          current={scaling ?? {}}
          nodegroupName={nodegroupName}
          onOpenChange={setShowScale}
        />
      )}
    </div>
  )
}

function AddNodegroupDialog({
  clusterName,
  vpcId,
  open,
  onOpenChange,
}: {
  clusterName: string
  vpcId: string | undefined
  open: boolean
  onOpenChange: (open: boolean) => void
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
    formState: { errors },
    handleSubmit,
    register,
    reset,
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

  const selectedSubnets = useWatch({ control, name: "subnetIds" })
  const selectedSubnetSet = new Set(selectedSubnets)
  const selectedInstanceType = useWatch({ control, name: "instanceTypes" })
  const selectedIsGpu = isGpuNodegroup(selectedInstanceType, instanceTypes)

  const toggleSubnet = (subnetId: string) => {
    const next = selectedSubnets.includes(subnetId)
      ? selectedSubnets.filter((id) => id !== subnetId)
      : [...selectedSubnets, subnetId]

    setValue("subnetIds", next, { shouldValidate: true })
  }

  const onSubmit = async (data: CreateNodegroupFormValues) => {
    await createNodegroup.mutateAsync({
      clusterName,
      nodegroupName: data.nodegroupName,
      nodeRole: data.nodeRole,
      subnetIds: data.subnetIds,
      instanceTypes: data.instanceTypes
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean),
      amiType: data.amiType,
      capacityType: data.capacityType,
      diskSize: data.diskSize,
      minSize: data.minSize,
      desiredSize: data.desiredSize,
      maxSize: data.maxSize,
    })
    reset()
    onOpenChange(false)
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent className="max-h-[85vh] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>Add node group</AlertDialogTitle>
        </AlertDialogHeader>

        <form className="space-y-4" id="add-nodegroup-form">
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

          <Field>
            <FieldTitle>
              <label htmlFor="ng-role">Node IAM role</label>
            </FieldTitle>
            <select
              aria-invalid={!!errors.nodeRole}
              className="h-7 w-full rounded-md border border-input bg-input/20 px-2 text-sm"
              id="ng-role"
              {...register("nodeRole")}
            >
              <option value="">Select role</option>
              {roles.map((role) => (
                <option key={role.Arn} value={role.Arn ?? ""}>
                  {role.RoleName}
                </option>
              ))}
            </select>
            <FieldError errors={[errors.nodeRole]} />
          </Field>

          <Field>
            <FieldTitle>Subnets</FieldTitle>
            {subnets.length === 0 ? (
              <p className="text-xs text-muted-foreground">
                No subnets in the cluster VPC.
              </p>
            ) : (
              <div className="space-y-1">
                {subnets.map((subnet) => (
                  <label
                    className="flex items-center gap-2 text-xs"
                    key={subnet.SubnetId}
                  >
                    <input
                      aria-label={`Subnet ${subnet.SubnetId}`}
                      checked={selectedSubnetSet.has(subnet.SubnetId ?? "")}
                      onChange={() => {
                        toggleSubnet(subnet.SubnetId ?? "")
                      }}
                      type="checkbox"
                    />
                    <span className="font-mono">
                      {subnet.SubnetId} · {subnet.CidrBlock}
                    </span>
                  </label>
                ))}
              </div>
            )}
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
            <Field>
              <FieldTitle>
                <label htmlFor="ng-ami">AMI type</label>
              </FieldTitle>
              <select
                className="h-7 w-full rounded-md border border-input bg-input/20 px-2 text-sm"
                id="ng-ami"
                {...register("amiType")}
              >
                {EKS_AMI_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="ng-capacity">Capacity type</label>
              </FieldTitle>
              <select
                className="h-7 w-full rounded-md border border-input bg-input/20 px-2 text-sm"
                id="ng-capacity"
                {...register("capacityType")}
              >
                {EKS_CAPACITY_TYPES.map((t) => (
                  <option key={t} value={t}>
                    {t}
                  </option>
                ))}
              </select>
            </Field>
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
        </form>

        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={createNodegroup.isPending}
            onClick={(e) => {
              e.preventDefault()
              void handleSubmit(onSubmit)()
            }}
          >
            {createNodegroup.isPending ? "Creating…" : "Create"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

export function NodegroupsTab({
  clusterName,
  clusterVersion,
  vpcId,
}: {
  clusterName: string
  clusterVersion: string | undefined
  vpcId: string | undefined
}) {
  const { data } = useSuspenseQuery(eksNodegroupsQueryOptions(clusterName))
  const deleteNodegroup = useDeleteNodegroup()
  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<string | undefined>()

  const nodegroups = data.nodegroups ?? []

  return (
    <>
      <PanelSection title={`Node groups (${nodegroups.length})`}>
        <div className="flex flex-col gap-3 py-1">
          <div>
            <Button
              onClick={() => {
                setShowAdd(true)
              }}
              size="sm"
              variant="outline"
            >
              <Plus data-icon="inline-start" />
              Add node group
            </Button>
          </div>
          {nodegroups.length > 0 ? (
            nodegroups.map((ng) => (
              <NodegroupRow
                clusterName={clusterName}
                clusterVersion={clusterVersion}
                key={ng}
                nodegroupName={ng}
                onDelete={setPendingDelete}
              />
            ))
          ) : (
            <p className="text-sm text-muted-foreground">No node groups.</p>
          )}
        </div>
      </PanelSection>

      <AddNodegroupDialog
        clusterName={clusterName}
        onOpenChange={setShowAdd}
        open={showAdd}
        vpcId={vpcId}
      />

      <DeleteConfirmationDialog
        description={`Delete node group "${pendingDelete ?? ""}"? This terminates its nodes.`}
        isPending={deleteNodegroup.isPending}
        onConfirm={() => {
          if (!pendingDelete) {
            return
          }

          deleteNodegroup.mutate(
            { clusterName, nodegroupName: pendingDelete },
            {
              onSuccess: () => {
                setPendingDelete(undefined)
              },
            },
          )
        }}
        onOpenChange={(o) => {
          if (!o) {
            setPendingDelete(undefined)
          }
        }}
        open={pendingDelete !== undefined}
        title="Delete node group"
      />
    </>
  )
}
