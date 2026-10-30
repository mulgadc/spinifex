import { zodResolver } from "@hookform/resolvers/zod"
import { useQueryClient, useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { SwitchList } from "@/components/switch-list"
import { SystemImageRequired } from "@/components/system-image-required"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { VpcSelectField } from "@/components/vpc-select-field"
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

import { CreateClusterRoleSection } from "./create-cluster-role-section"

interface CreateClusterPageProps {
  // Called with the new cluster's name, or with nothing on cancel.
  onClose: (clusterName?: string) => void
}

export function CreateClusterPage({ onClose }: CreateClusterPageProps) {
  const queryClient = useQueryClient()
  const [isRoleFormOpen, setIsRoleFormOpen] = useState(false)
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: rolesData } = useSuspenseQuery(iamRolesQueryOptions)
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)
  const createCluster = useCreateCluster()

  const hasEksSystemImage = (imagesData.Images ?? []).some(isEksSystemImage)

  const vpcs = vpcsData.Vpcs ?? []
  const allSubnets = subnetsData.Subnets ?? []
  const roles = rolesData.Roles ?? []

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
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

  const selectedVpc = useWatch({ control, name: "vpcId" })
  const selectedSubnets = useWatch({ control, name: "subnetIds" })
  const publicAccess = useWatch({ control, name: "endpointPublicAccess" })
  const publicCidrs = useWatch({ control, name: "publicAccessCidrs" })

  const vpcSubnets = allSubnets.filter((s) => s.VpcId === selectedVpc)

  const handleVpcChange = (newVpcId: string | null = "") => {
    setValue("vpcId", newVpcId ?? "", { shouldValidate: true })
    setValue("subnetIds", [])
  }

  const updateCidr = (index: number, value: string) => {
    const next = [...publicCidrs]
    next[index] = value
    setValue("publicAccessCidrs", next, { shouldValidate: true })
  }

  const addCidr = () => {
    setValue("publicAccessCidrs", [...publicCidrs, ""], {
      shouldValidate: true,
    })
  }

  const removeCidr = (index: number) => {
    setValue(
      "publicAccessCidrs",
      publicCidrs.filter((_, i) => i !== index),
      { shouldValidate: true },
    )
  }

  const clusterName = useWatch({ control, name: "name" })

  const [selectedRoleArn, version, privateAccess, creatorAdmin] = useWatch({
    control,
    name: [
      "roleArn",
      "version",
      "endpointPrivateAccess",
      "bootstrapClusterCreatorAdminPermissions",
    ],
  })

  const handleRoleCreated = async (roleArn: string) => {
    await queryClient.invalidateQueries({
      queryKey: iamRolesQueryOptions.queryKey,
    })
    setValue("roleArn", roleArn, { shouldValidate: true })
    setIsRoleFormOpen(false)
  }

  const onSubmit = async (data: CreateClusterFormData) => {
    try {
      await createCluster.mutateAsync(data)
      onClose(data.name)
    } catch {
      // Left open, with the refusal shown beside the submit button.
    }
  }

  if (!hasEksSystemImage) {
    return (
      <SystemImageRequired
        description="EKS clusters boot a Spinifex-managed node image (K3s) that is not shipped with the platform. Import it before creating a cluster."
        importCommand="spx admin images import --name spinifex-eks-node"
        title="EKS system image not found"
      />
    )
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createCluster.isPending || isSubmitting}
      >
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
              aria-expanded={isRoleFormOpen}
              onClick={() => {
                setIsRoleFormOpen(!isRoleFormOpen)
              }}
              type="button"
              variant="outline"
            >
              Create role
            </Button>
          </div>
          <FieldError errors={[errors.roleArn]} />
        </Field>

        {isRoleFormOpen && (
          <CreateClusterRoleSection
            clusterName={clusterName}
            onCancel={() => {
              setIsRoleFormOpen(false)
            }}
            onCreated={handleRoleCreated}
          />
        )}

        <VpcSelectField
          control={control}
          id="cluster-vpc"
          name="vpcId"
          onValueChange={handleVpcChange}
          placeholder="Select VPC"
          vpcs={vpcs}
        />

        <Field>
          <FieldTitle>Subnets (select at least 1)</FieldTitle>
          <SwitchList
            emptyText="No subnets in the selected VPC."
            itemLabel="Subnet"
            items={vpcSubnets.map((subnet) => ({
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
            onChange={(next) => {
              setValue("subnetIds", next, { shouldValidate: true })
            }}
            selected={selectedSubnets}
          />
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
              <div className="mt-2 flex items-center gap-2 text-xs">
                <Switch
                  aria-label="Enable public access"
                  checked={field.value}
                  id="cluster-public-access"
                  onCheckedChange={field.onChange}
                />
                <Label htmlFor="cluster-public-access">Public</Label>
              </div>
            )}
          />
          <Controller
            control={control}
            name="endpointPrivateAccess"
            render={({ field }) => (
              <div className="flex items-center gap-2 text-xs">
                <Switch
                  aria-label="Enable private access"
                  checked={field.value}
                  id="cluster-private-access"
                  onCheckedChange={field.onChange}
                />
                <Label htmlFor="cluster-private-access">Private</Label>
              </div>
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
                // oxlint-disable-next-line no-array-index-key -- controlled inputs; a stable key needs publicAccessCidrs off string[]
                <div className="flex items-center gap-2" key={index}>
                  <Input
                    aria-label={`Public access CIDR ${index + 1}`}
                    variant="code"
                    onChange={(e) => {
                      updateCidr(index, e.target.value)
                    }}
                    placeholder="203.0.113.0/24"
                    value={cidr}
                  />
                  <Button
                    onClick={() => {
                      removeCidr(index)
                    }}
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
              <div className="mt-2 flex items-center gap-2 text-xs">
                <Switch
                  aria-label="Grant the cluster creator admin permissions"
                  checked={field.value}
                  id="cluster-creator-admin"
                  onCheckedChange={field.onChange}
                />
                <Label htmlFor="cluster-creator-admin">
                  Grant the cluster creator admin permissions
                </Label>
              </div>
            )}
          />
        </Field>

        <CliCommandPanel
          commands={buildCreateClusterCommands({
            name: clusterName,
            roleArn: selectedRoleArn,
            version,
            subnetIds: selectedSubnets,
            publicAccess,
            privateAccess,
            publicCidrs,
            creatorAdmin,
          })}
        />

        <FormActions
          error={createCluster.error}
          errorMsg="Failed to create cluster"
          isPending={createCluster.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create cluster"
        />
      </FormLock>
    </form>
  )
}

interface CreateClusterCommandInput {
  name: string
  roleArn: string
  version: string
  subnetIds: string[]
  publicAccess: boolean
  privateAccess: boolean
  publicCidrs: string[]
  creatorAdmin: boolean
}

// Mirrors the request: CIDRs go only with public access, and authentication
// is always API access entries.
function buildCreateClusterCommands({
  name,
  roleArn,
  version,
  subnetIds,
  publicAccess,
  privateAccess,
  publicCidrs,
  creatorAdmin,
}: CreateClusterCommandInput): CliCommand[] {
  const vpcConfig = [
    `subnetIds=${subnetIds.length > 0 ? subnetIds.join(",") : "<subnet-ids>"}`,
    `endpointPublicAccess=${publicAccess}`,
    `endpointPrivateAccess=${privateAccess}`,
  ]

  if (publicAccess && publicCidrs.length > 0) {
    vpcConfig.push(`publicAccessCidrs=${publicCidrs.join(",")}`)
  }

  return [
    {
      label: "Create cluster",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws eks create-cluster" },
        ...commandFlag("\\\n  --name", cliPlaceholder(name.trim(), "name")),
        ...commandFlag("\\\n  --role-arn", cliPlaceholder(roleArn, "role-arn")),
        ...optionalFlag("\\\n  --kubernetes-version", version),
        ...commandFlag(
          "\\\n  --resources-vpc-config",
          shellSingleQuote(vpcConfig.join(",")),
        ),
        ...commandFlag(
          "\\\n  --access-config",
          `authenticationMode=API,bootstrapClusterCreatorAdminPermissions=${creatorAdmin}`,
        ),
      ],
    },
  ]
}
