import type { Subnet, Vpc } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { getNameTag } from "@/lib/utils"
import { useCreateCluster } from "@/mutations/eks"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { iamRolesQueryOptions } from "@/queries/iam"
import {
  type CreateClusterFormData,
  createClusterSchema,
  EKS_SUPPORTED_VERSIONS,
} from "@/types/eks"

function vpcLabel(vpc: Vpc): string {
  const name = getNameTag(vpc.Tags)
  return name ? `${vpc.VpcId} (${name})` : `${vpc.VpcId} (${vpc.CidrBlock})`
}

function subnetLabel(subnet: Subnet): string {
  const name = getNameTag(subnet.Tags)
  const suffix = name ? `${subnet.SubnetId} (${name})` : subnet.SubnetId
  return `${suffix} · ${subnet.CidrBlock}`
}

export function CreateClusterPage() {
  const navigate = useNavigate()
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: sgsData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
  const { data: rolesData } = useSuspenseQuery(iamRolesQueryOptions)
  const createCluster = useCreateCluster()

  const vpcs = vpcsData.Vpcs ?? []
  const allSubnets = subnetsData.Subnets ?? []
  const allSgs = sgsData.SecurityGroups ?? []
  const roles = rolesData.Roles ?? []

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
    watch,
  } = useForm<CreateClusterFormData>({
    resolver: zodResolver(createClusterSchema),
    defaultValues: {
      name: "",
      version: EKS_SUPPORTED_VERSIONS[0],
      roleArn: "",
      vpcId: "",
      subnetIds: [],
      securityGroupIds: [],
      bootstrapClusterCreatorAdminPermissions: true,
    },
  })

  const selectedVpc = watch("vpcId")
  const selectedSubnets = watch("subnetIds")
  const selectedSgs = watch("securityGroupIds")

  const vpcSubnets = allSubnets.filter((s) => s.VpcId === selectedVpc)
  const vpcSgs = allSgs.filter((g) => g.VpcId === selectedVpc)

  const handleVpcChange = (newVpcId: string | null = "") => {
    setValue("vpcId", newVpcId ?? "", { shouldValidate: true })
    setValue("subnetIds", [])
    setValue("securityGroupIds", [])
  }

  const toggleSubnet = (subnetId: string) => {
    const next = selectedSubnets.includes(subnetId)
      ? selectedSubnets.filter((id) => id !== subnetId)
      : [...selectedSubnets, subnetId]
    setValue("subnetIds", next, { shouldValidate: true })
  }

  const toggleSg = (sgId: string) => {
    const next = selectedSgs.includes(sgId)
      ? selectedSgs.filter((id) => id !== sgId)
      : [...selectedSgs, sgId]
    setValue("securityGroupIds", next)
  }

  const onSubmit = async (data: CreateClusterFormData) => {
    await createCluster.mutateAsync(data)
    await navigate({
      to: "/eks/list-clusters/$clusterName",
      params: { clusterName: data.name },
    })
  }

  return (
    <>
      <BackLink to="/eks/list-clusters">Clusters</BackLink>
      <PageHeading subtitle="EKS" title="Create Cluster" />

      {createCluster.error && (
        <ErrorBanner
          error={createCluster.error}
          msg="Failed to create cluster"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="cluster-name">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.name}
            id="cluster-name"
            placeholder="my-cluster"
            {...register("name")}
          />
          <FieldError errors={[errors.name]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="cluster-version">Kubernetes version</label>
          </FieldTitle>
          <Controller
            control={control}
            name="version"
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value}>
                <SelectTrigger className="w-full" id="cluster-version">
                  <SelectValue placeholder="Select version" />
                </SelectTrigger>
                <SelectContent>
                  {EKS_SUPPORTED_VERSIONS.map((v) => (
                    <SelectItem key={v} value={v}>
                      {v}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.version]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="cluster-role">Cluster IAM role</label>
          </FieldTitle>
          <Controller
            control={control}
            name="roleArn"
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value}>
                <SelectTrigger
                  aria-invalid={!!errors.roleArn}
                  className="w-full"
                  id="cluster-role"
                >
                  <SelectValue placeholder="Select role" />
                </SelectTrigger>
                <SelectContent>
                  {roles.map((role) => (
                    <SelectItem key={role.Arn} value={role.Arn ?? ""}>
                      {role.RoleName}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.roleArn]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="cluster-vpc">VPC</label>
          </FieldTitle>
          <Controller
            control={control}
            name="vpcId"
            render={({ field }) => (
              <Select
                onValueChange={(v) => handleVpcChange(v)}
                value={field.value}
              >
                <SelectTrigger
                  aria-invalid={!!errors.vpcId}
                  className="w-full"
                  id="cluster-vpc"
                >
                  <SelectValue placeholder="Select VPC" />
                </SelectTrigger>
                <SelectContent>
                  {vpcs.map((vpc) => (
                    <SelectItem key={vpc.VpcId} value={vpc.VpcId ?? ""}>
                      {vpcLabel(vpc)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <FieldError errors={[errors.vpcId]} />
        </Field>

        <Field>
          <FieldTitle>Subnets (select at least 1)</FieldTitle>
          {vpcSubnets.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No subnets in the selected VPC.
            </p>
          ) : (
            <div className="space-y-1">
              {vpcSubnets.map((subnet) => (
                <label
                  className="flex items-center gap-2 text-xs"
                  key={subnet.SubnetId}
                >
                  <input
                    aria-label={`Subnet ${subnetLabel(subnet)}`}
                    checked={selectedSubnets.includes(subnet.SubnetId ?? "")}
                    onChange={() => toggleSubnet(subnet.SubnetId ?? "")}
                    type="checkbox"
                  />
                  <span className="font-mono">{subnetLabel(subnet)}</span>
                </label>
              ))}
            </div>
          )}
          <FieldError errors={[errors.subnetIds]} />
        </Field>

        <Field>
          <FieldTitle>Control-plane security groups</FieldTitle>
          {vpcSgs.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No security groups in the selected VPC.
            </p>
          ) : (
            <div className="space-y-1">
              {vpcSgs.map((sg) => (
                <label
                  className="flex items-center gap-2 text-xs"
                  key={sg.GroupId}
                >
                  <input
                    aria-label={`Security group ${sg.GroupId} (${sg.GroupName})`}
                    checked={selectedSgs.includes(sg.GroupId ?? "")}
                    onChange={() => toggleSg(sg.GroupId ?? "")}
                    type="checkbox"
                  />
                  <span className="font-mono">
                    {sg.GroupId} ({sg.GroupName})
                  </span>
                </label>
              ))}
            </div>
          )}
        </Field>

        <Field>
          <FieldTitle>Access</FieldTitle>
          <p className="text-xs text-muted-foreground">
            Authentication mode is <span className="font-mono">API</span> (IAM
            access entries). The legacy aws-auth ConfigMap is not supported.
          </p>
          <Controller
            control={control}
            name="bootstrapClusterCreatorAdminPermissions"
            render={({ field }) => (
              <label className="mt-2 flex items-center gap-2 text-xs">
                <input
                  aria-label="Grant the cluster creator admin permissions"
                  checked={field.value}
                  onChange={(e) => field.onChange(e.target.checked)}
                  type="checkbox"
                />
                Grant the cluster creator admin permissions
              </label>
            )}
          />
        </Field>

        <FormActions
          isPending={createCluster.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () => await navigate({ to: "/eks/list-clusters" })}
          pendingLabel="Creating…"
          submitLabel="Create Cluster"
        />
      </form>
    </>
  )
}
