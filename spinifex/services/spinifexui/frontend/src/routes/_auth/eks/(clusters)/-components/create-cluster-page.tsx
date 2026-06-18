import type { Subnet, Vpc } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQueryClient, useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { useState } from "react"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { isEksSystemImage } from "@/lib/system-managed"
import { getNameTag } from "@/lib/utils"
import { useCreateCluster } from "@/mutations/eks"
import {
  ec2ImagesQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import { iamRolesQueryOptions } from "@/queries/iam"
import {
  type CreateClusterFormData,
  createClusterSchema,
  EKS_SUPPORTED_VERSIONS,
} from "@/types/eks"

import { CreateClusterRoleDialog } from "./create-cluster-role-dialog"
import { EksSystemImageRequired } from "./eks-system-image-required"

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
  const queryClient = useQueryClient()
  const [isRechecking, setIsRechecking] = useState(false)
  const [isRoleDialogOpen, setIsRoleDialogOpen] = useState(false)
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: rolesData } = useSuspenseQuery(iamRolesQueryOptions)
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)
  const createCluster = useCreateCluster()

  const hasEksSystemImage = (imagesData.Images ?? []).some(isEksSystemImage)

  const handleRecheck = async () => {
    setIsRechecking(true)
    try {
      await queryClient.invalidateQueries({
        queryKey: ec2ImagesQueryOptions.queryKey,
      })
    } finally {
      setIsRechecking(false)
    }
  }

  const vpcs = vpcsData.Vpcs ?? []
  const allSubnets = subnetsData.Subnets ?? []
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
      bootstrapClusterCreatorAdminPermissions: true,
      endpointPublicAccess: true,
      endpointPrivateAccess: false,
      publicAccessCidrs: ["0.0.0.0/0"],
    },
  })

  const selectedVpc = watch("vpcId")
  const selectedSubnets = watch("subnetIds")
  const publicAccess = watch("endpointPublicAccess")
  const publicCidrs = watch("publicAccessCidrs")

  const vpcSubnets = allSubnets.filter((s) => s.VpcId === selectedVpc)

  const handleVpcChange = (newVpcId: string | null = "") => {
    setValue("vpcId", newVpcId ?? "", { shouldValidate: true })
    setValue("subnetIds", [])
  }

  const toggleSubnet = (subnetId: string) => {
    const next = selectedSubnets.includes(subnetId)
      ? selectedSubnets.filter((id) => id !== subnetId)
      : [...selectedSubnets, subnetId]
    setValue("subnetIds", next, { shouldValidate: true })
  }

  const updateCidr = (index: number, value: string) => {
    const next = [...publicCidrs]
    next[index] = value
    setValue("publicAccessCidrs", next, { shouldValidate: true })
  }

  const addCidr = () =>
    setValue("publicAccessCidrs", [...publicCidrs, ""], {
      shouldValidate: true,
    })

  const removeCidr = (index: number) =>
    setValue(
      "publicAccessCidrs",
      publicCidrs.filter((_, i) => i !== index),
      { shouldValidate: true },
    )

  const clusterName = watch("name")

  const handleRoleCreated = async (roleArn: string) => {
    await queryClient.invalidateQueries({
      queryKey: iamRolesQueryOptions.queryKey,
    })
    setValue("roleArn", roleArn, { shouldValidate: true })
  }

  const onSubmit = async (data: CreateClusterFormData) => {
    await createCluster.mutateAsync(data)
    await navigate({
      to: "/eks/list-clusters/$clusterName",
      params: { clusterName: data.name },
    })
  }

  if (!hasEksSystemImage) {
    return (
      <>
        <BackLink to="/eks/list-clusters">Clusters</BackLink>
        <PageHeading subtitle="EKS" title="Create Cluster" />
        <EksSystemImageRequired
          isRechecking={isRechecking}
          onRecheck={handleRecheck}
        />
      </>
    )
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
          <div className="flex items-center gap-2">
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
            <Button
              onClick={() => setIsRoleDialogOpen(true)}
              type="button"
              variant="outline"
            >
              Create role
            </Button>
          </div>
          <FieldError errors={[errors.roleArn]} />
        </Field>

        <CreateClusterRoleDialog
          clusterName={clusterName}
          onCreated={handleRoleCreated}
          onOpenChange={setIsRoleDialogOpen}
          open={isRoleDialogOpen}
        />

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
          <FieldTitle>Endpoint access</FieldTitle>
          <p className="text-xs text-muted-foreground">
            Public access exposes the API server on an internet-facing endpoint;
            private access keeps it reachable only from within the VPC. At least
            one must be enabled.
          </p>
          <Controller
            control={control}
            name="endpointPublicAccess"
            render={({ field }) => (
              <label className="mt-2 flex items-center gap-2 text-xs">
                <input
                  aria-label="Enable public access"
                  checked={field.value}
                  onChange={(e) => field.onChange(e.target.checked)}
                  type="checkbox"
                />
                Public
              </label>
            )}
          />
          <Controller
            control={control}
            name="endpointPrivateAccess"
            render={({ field }) => (
              <label className="flex items-center gap-2 text-xs">
                <input
                  aria-label="Enable private access"
                  checked={field.value}
                  onChange={(e) => field.onChange(e.target.checked)}
                  type="checkbox"
                />
                Private
              </label>
            )}
          />
          <FieldError errors={[errors.endpointPublicAccess]} />

          {publicAccess && (
            <div className="mt-2 space-y-2">
              <p className="text-xs text-muted-foreground">
                Public access source ranges (CIDR). Defaults to{" "}
                <span className="font-mono">0.0.0.0/0</span>.
              </p>
              {publicCidrs.map((cidr, index) => (
                // eslint-disable-next-line react/no-array-index-key
                <div className="flex items-center gap-2" key={index}>
                  <Input
                    aria-label={`Public access CIDR ${index + 1}`}
                    className="font-mono"
                    onChange={(e) => updateCidr(index, e.target.value)}
                    placeholder="203.0.113.0/24"
                    value={cidr}
                  />
                  <Button
                    onClick={() => removeCidr(index)}
                    size="sm"
                    type="button"
                    variant="ghost"
                  >
                    Remove
                  </Button>
                </div>
              ))}
              <Button
                onClick={addCidr}
                size="sm"
                type="button"
                variant="outline"
              >
                Add CIDR
              </Button>
              <FieldError errors={[errors.publicAccessCidrs]} />
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
