import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Trash2 } from "lucide-react"
import { useState } from "react"
import { useForm } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateNodegroup, useDeleteNodegroup } from "@/mutations/eks"
import { ec2SubnetsQueryOptions } from "@/queries/ec2"
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

function NodegroupRow({
  clusterName,
  nodegroupName,
  onDelete,
}: {
  clusterName: string
  nodegroupName: string
  onDelete: (name: string) => void
}) {
  const { data } = useQuery(
    eksNodegroupQueryOptions(clusterName, nodegroupName),
  )
  const ng = data?.nodegroup
  const scaling = ng?.scalingConfig

  return (
    <DetailCard>
      <DetailCard.Header>
        <div className="flex items-center justify-between">
          <span>{nodegroupName}</span>
          <Button
            onClick={() => onDelete(nodegroupName)}
            size="icon"
            variant="ghost"
          >
            <Trash2 className="size-4" />
          </Button>
        </div>
      </DetailCard.Header>
      <DetailCard.Content>
        <DetailRow label="Status" value={ng?.status} />
        <DetailRow
          label="Instance types"
          value={ng?.instanceTypes?.join(", ")}
        />
        <DetailRow label="AMI type" value={ng?.amiType} />
        <DetailRow label="Capacity type" value={ng?.capacityType} />
        <DetailRow
          label="Scaling (min / desired / max)"
          value={
            scaling
              ? `${scaling.minSize} / ${scaling.desiredSize} / ${scaling.maxSize}`
              : undefined
          }
        />
      </DetailCard.Content>
    </DetailCard>
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
  const createNodegroup = useCreateNodegroup()

  const roles = rolesData?.Roles ?? []
  const subnets = (subnetsData?.Subnets ?? []).filter((s) => s.VpcId === vpcId)

  const {
    formState: { errors },
    handleSubmit,
    register,
    reset,
    setValue,
    watch,
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

  const selectedSubnets = watch("subnetIds")

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
                      checked={selectedSubnets.includes(subnet.SubnetId ?? "")}
                      onChange={() => toggleSubnet(subnet.SubnetId ?? "")}
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
              <label htmlFor="ng-instance-types">Instance types</label>
            </FieldTitle>
            <Input
              id="ng-instance-types"
              placeholder="t3.medium, t3.large"
              {...register("instanceTypes")}
            />
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
  vpcId,
}: {
  clusterName: string
  vpcId: string | undefined
}) {
  const { data } = useSuspenseQuery(eksNodegroupsQueryOptions(clusterName))
  const deleteNodegroup = useDeleteNodegroup()
  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<string | undefined>()

  const nodegroups = data.nodegroups ?? []

  return (
    <>
      <div className="mb-4 flex justify-end">
        <Button onClick={() => setShowAdd(true)} size="sm">
          Add node group
        </Button>
      </div>

      {nodegroups.length > 0 ? (
        <div className="space-y-4">
          {nodegroups.map((ng) => (
            <NodegroupRow
              clusterName={clusterName}
              key={ng}
              nodegroupName={ng}
              onDelete={setPendingDelete}
            />
          ))}
        </div>
      ) : (
        <p className="text-muted-foreground">No node groups.</p>
      )}

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
            { onSuccess: () => setPendingDelete(undefined) },
          )
        }}
        onOpenChange={(o) => !o && setPendingDelete(undefined)}
        open={pendingDelete !== undefined}
        title="Delete node group"
      />
    </>
  )
}
