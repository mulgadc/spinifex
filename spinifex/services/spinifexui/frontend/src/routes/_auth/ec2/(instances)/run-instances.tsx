import type { LaunchTemplateVersion } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { ChevronDown } from "lucide-react"
import { useEffect } from "react"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { formatVRAMMiB } from "@/lib/utils"
import { useCreateInstance } from "@/mutations/ec2"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2LaunchTemplatesQueryOptions,
  ec2LaunchTemplateVersionsQueryOptions,
  ec2PlacementGroupsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import {
  type CreateInstanceFormData,
  type CreateInstanceParams,
  VOLUME_TYPES,
  createInstanceSchema,
  nextLaunchWizardName,
} from "@/types/ec2"

const DEFAULT_ROOT_DEVICE_NAME = "/dev/sda1"
const DEFAULT_VOLUME_TYPE = "gp3"
const MIN_VOLUME_SIZE_GIB = 1
const MAX_VOLUME_SIZE_GIB = 16_384

export const Route = createFileRoute("/_auth/ec2/(instances)/run-instances")({
  validateSearch: (
    search: Record<string, unknown>,
  ): { launchTemplateId?: string; launchTemplateVersion?: string } => ({
    launchTemplateId:
      typeof search.launchTemplateId === "string"
        ? search.launchTemplateId
        : undefined,
    launchTemplateVersion:
      typeof search.launchTemplateVersion === "string"
        ? search.launchTemplateVersion
        : undefined,
  }),
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2ImagesQueryOptions),
      context.queryClient.ensureQueryData(ec2KeyPairsQueryOptions),
      context.queryClient.ensureQueryData(ec2InstanceTypesQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2PlacementGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2VpcsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2LaunchTemplatesQueryOptions),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Run Instances | EC2 | Mulga",
      },
    ],
  }),
  component: CreateInstance,
})

function CreateInstance() {
  const navigate = useNavigate()
  const search = Route.useSearch()
  const { data: imagesData } = useSuspenseQuery(ec2ImagesQueryOptions)
  const { data: keyPairsData } = useSuspenseQuery(ec2KeyPairsQueryOptions)
  const { data: instanceTypesData } = useSuspenseQuery(
    ec2InstanceTypesQueryOptions,
  )
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: pgData } = useSuspenseQuery(ec2PlacementGroupsQueryOptions)
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const { data: sgData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
  const { data: ltData } = useSuspenseQuery(ec2LaunchTemplatesQueryOptions)
  const createMutation = useCreateInstance()
  const images = imagesData.Images ?? []
  const keyPairs = keyPairsData.KeyPairs ?? []
  const subnets = subnetsData.Subnets ?? []
  const placementGroups = pgData.PlacementGroups ?? []
  const vpcs = vpcsData.Vpcs ?? []
  const securityGroups = sgData.SecurityGroups ?? []
  const launchTemplates = ltData.LaunchTemplates ?? []
  const defaultVpcId = (vpcs.find((v) => v.IsDefault) ?? vpcs[0])?.VpcId
  const defaultSgName = nextLaunchWizardName(
    securityGroups.map((sg) => sg.GroupName ?? ""),
  )
  const instanceTypeCounts: Record<string, number> = {}
  const gpuInfoByType = new Map<string, { totalMiB: number; gpuName: string }>()
  for (const type of instanceTypesData.InstanceTypes ?? []) {
    const typeName = type.InstanceType
    if (!typeName) {
      continue
    }
    instanceTypeCounts[typeName] = (instanceTypeCounts[typeName] ?? 0) + 1
    if (!gpuInfoByType.has(typeName)) {
      const gpus = type.GpuInfo?.Gpus
      if (gpus && gpus.length > 0) {
        gpuInfoByType.set(typeName, {
          totalMiB: type.GpuInfo?.TotalGpuMemoryInMiB ?? 0,
          gpuName: gpus[0]?.Name ?? "",
        })
      }
    }
  }

  const uniqueInstanceTypes = Object.keys(instanceTypeCounts).toSorted()
  const gpuInstanceTypes = uniqueInstanceTypes.filter((t) =>
    gpuInfoByType.has(t),
  )
  const standardInstanceTypes = uniqueInstanceTypes.filter(
    (t) => !gpuInfoByType.has(t),
  )

  // Compute default values from loaded data
  const defaultImageId = images[0]?.ImageId
  const defaultKeyName = keyPairs[0]?.KeyName
  const defaultInstanceType =
    uniqueInstanceTypes.find((type) => type.endsWith(".nano")) ??
    uniqueInstanceTypes[0]
  const defaultRoot = getRootMapping(
    images.find((img) => img.ImageId === defaultImageId),
  )

  const {
    control,
    handleSubmit,
    register,
    setValue,
    getValues,
    watch,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(
      createInstanceSchema
        .refine(
          (data) =>
            !data.instanceType ||
            data.count <= (instanceTypeCounts[data.instanceType] ?? 1),
          {
            message: "Cannot exceed available capacity",
            path: ["count"],
          },
        )
        .superRefine((data, ctx) => {
          const minSize = getRootMapping(
            images.find((img) => img.ImageId === data.imageId),
          ).snapshotSize
          if (
            data.rootVolumeSize !== undefined &&
            minSize !== undefined &&
            data.rootVolumeSize < minSize
          ) {
            ctx.addIssue({
              code: "custom",
              message: `Volume size must be at least ${minSize} GiB (AMI snapshot size)`,
              path: ["rootVolumeSize"],
            })
          }
        }),
    ),
    defaultValues: {
      count: 1,
      imageId: defaultImageId ?? "",
      keyName: defaultKeyName ?? "",
      instanceType: defaultInstanceType ?? "",
      launchTemplateId: search.launchTemplateId,
      launchTemplateVersion: search.launchTemplateVersion ?? "$Default",
      rootDeviceName: defaultRoot.deviceName,
      securityGroupMode: "create",
      securityGroupIds: [],
      newSgName: defaultSgName,
      newSgDescription: "Created by launch wizard",
      allowSsh: true,
      allowHttp: false,
      allowHttps: false,
      ruleSource: "anywhere",
      customCidr: "",
    },
  })

  const selectedInstanceType = watch("instanceType")
  const maxCount = selectedInstanceType
    ? (instanceTypeCounts[selectedInstanceType] ?? 1)
    : 1
  const selectedGPUInfo = selectedInstanceType
    ? gpuInfoByType.get(selectedInstanceType)
    : undefined
  const selectedImageId = watch("imageId")
  const selectedRoot = getRootMapping(
    images.find((img) => img.ImageId === selectedImageId),
  )

  const selectedSubnetId = watch("subnetId")
  const sgMode = watch("securityGroupMode")
  const ruleSource = watch("ruleSource")
  const selectedSgIds = watch("securityGroupIds") ?? []

  // Launch template: when one is selected the instance is launched from it and
  // the direct-config fields below are hidden — the template supplies them.
  const selectedTemplateId = watch("launchTemplateId")
  const selectedTemplateVersion = watch("launchTemplateVersion")
  const templateVersionsQuery = useQuery({
    ...ec2LaunchTemplateVersionsQueryOptions(selectedTemplateId ?? ""),
    enabled: !!selectedTemplateId,
  })
  const templateVersions =
    templateVersionsQuery.data?.LaunchTemplateVersions ?? []
  const resolvedTemplateData = resolveTemplateVersion(
    templateVersions,
    selectedTemplateVersion,
  )?.LaunchTemplateData
  const effectiveVpcId =
    subnets.find((s) => s.SubnetId === selectedSubnetId)?.VpcId ?? defaultVpcId
  const vpcSecurityGroups = effectiveVpcId
    ? securityGroups.filter((sg) => sg.VpcId === effectiveVpcId)
    : securityGroups

  const toggleSg = (sgId: string) => {
    const current = getValues("securityGroupIds") ?? []
    const next = current.includes(sgId)
      ? current.filter((id) => id !== sgId)
      : [...current, sgId]
    setValue("securityGroupIds", next, { shouldValidate: true })
  }

  // Re-prefill DeviceName when the user picks a different AMI. Size / type /
  // delete-on-termination stay blank by default so an unchanged form sends
  // no BlockDeviceMappings (preserves today's backend default).
  useEffect(() => {
    setValue("rootDeviceName", selectedRoot.deviceName)
  }, [selectedImageId, selectedRoot.deviceName, setValue])

  const onSubmit = async (data: CreateInstanceFormData) => {
    // Launch wholly from the template (plus count); clear direct-config fields
    // so nothing overrides what the template defines.
    const params: CreateInstanceParams = data.launchTemplateId
      ? {
          ...data,
          imageId: undefined,
          instanceType: undefined,
          keyName: undefined,
          subnetId: undefined,
          placementGroupName: undefined,
          securityGroupMode: undefined,
          securityGroupIds: [],
          resolvedVpcId: effectiveVpcId,
        }
      : { ...data, resolvedVpcId: effectiveVpcId }
    await createMutation.mutateAsync(params)
    navigate({ to: "/ec2/describe-instances" })
  }

  return (
    <>
      <BackLink to="/ec2/describe-instances">Back to instances</BackLink>
      <PageHeading title="Run Instances" />

      {/* Handle error when no instance types available */}
      {uniqueInstanceTypes.length === 0 && !selectedTemplateId && (
        <ErrorBanner msg="No compute available. No new instances can be created until compute is available." />
      )}

      {/* Handle error after submission */}
      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create instance"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        {/* Launch template */}
        <Field>
          <FieldTitle>
            <label htmlFor="launchTemplateId">Launch Template</label>
          </FieldTitle>
          <FieldDescription>
            Optionally launch from a saved template. Its configuration is used
            as-is.
          </FieldDescription>
          <Controller
            control={control}
            name="launchTemplateId"
            render={({ field }) => (
              <Select
                onValueChange={(value) =>
                  field.onChange(value === "none" ? undefined : value)
                }
                value={field.value ?? "none"}
              >
                <SelectTrigger className="w-full" id="launchTemplateId">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">none</SelectItem>
                  {launchTemplates.map((lt) => (
                    <SelectItem
                      key={lt.LaunchTemplateId}
                      value={lt.LaunchTemplateId ?? ""}
                    >
                      {lt.LaunchTemplateName ?? lt.LaunchTemplateId}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
        </Field>

        {selectedTemplateId && (
          <Field>
            <FieldTitle>
              <label htmlFor="launchTemplateVersion">Version</label>
            </FieldTitle>
            <Controller
              control={control}
              name="launchTemplateVersion"
              render={({ field }) => (
                <Select
                  onValueChange={(value) => field.onChange(value)}
                  value={field.value ?? "$Default"}
                >
                  <SelectTrigger className="w-full" id="launchTemplateVersion">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="$Default">$Default</SelectItem>
                    <SelectItem value="$Latest">$Latest</SelectItem>
                    {templateVersions.map((version) => (
                      <SelectItem
                        key={version.VersionNumber}
                        value={String(version.VersionNumber)}
                      >
                        v{version.VersionNumber}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            />
            {resolvedTemplateData && (
              <div className="mt-3 space-y-1 rounded-md border border-border p-3 text-xs">
                <p className="text-muted-foreground">
                  Configuration from this version:
                </p>
                <p>
                  Image:{" "}
                  <span className="font-mono">
                    {resolvedTemplateData.ImageId ?? "—"}
                  </span>
                </p>
                <p>
                  Instance type:{" "}
                  <span className="font-mono">
                    {resolvedTemplateData.InstanceType ?? "—"}
                  </span>
                </p>
                <p>
                  Key pair:{" "}
                  <span className="font-mono">
                    {resolvedTemplateData.KeyName ?? "—"}
                  </span>
                </p>
              </div>
            )}
          </Field>
        )}

        {!selectedTemplateId && (
          <>
            {/* ImageSelection */}
            <Field>
              <FieldTitle>
                <label htmlFor="imageId">Image</label>
              </FieldTitle>
              <Controller
                control={control}
                name="imageId"
                render={({ field }) => {
                  const selectedImage = images.find(
                    (img) => img.ImageId === field.value,
                  )
                  return (
                    <Select
                      onValueChange={(value) => field.onChange(value)}
                      value={field.value ?? ""}
                    >
                      <SelectTrigger
                        aria-invalid={!!errors.imageId}
                        className="w-full"
                        id="imageId"
                      >
                        <SelectValue>
                          {selectedImage
                            ? `${selectedImage.Name ?? "Unnamed"} (${selectedImage.Architecture})`
                            : ""}
                        </SelectValue>
                      </SelectTrigger>
                      <SelectContent>
                        {images.map((image) => (
                          <SelectItem
                            key={image.ImageId}
                            value={image.ImageId ?? ""}
                          >
                            {image.Name ?? "Unnamed"} ({image.Architecture})
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  )
                }}
              />
              <FieldError errors={[errors.imageId]} />
            </Field>

            {/* Instance Type */}
            <Field>
              <FieldTitle>
                <label htmlFor="instanceType">Instance Type</label>
              </FieldTitle>
              <Controller
                control={control}
                name="instanceType"
                render={({ field }) => (
                  <Select
                    onValueChange={(value) => field.onChange(value)}
                    value={field.value ?? ""}
                  >
                    <SelectTrigger
                      aria-invalid={!!errors.instanceType}
                      className="w-full"
                      id="instanceType"
                    >
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {standardInstanceTypes.length > 0 && (
                        <SelectGroup>
                          {gpuInstanceTypes.length > 0 && (
                            <SelectLabel>Standard</SelectLabel>
                          )}
                          {standardInstanceTypes.map((type) => (
                            <SelectItem key={type} value={type}>
                              {type} ({instanceTypeCounts[type]} available)
                            </SelectItem>
                          ))}
                        </SelectGroup>
                      )}
                      {gpuInstanceTypes.length > 0 &&
                        standardInstanceTypes.length > 0 && <SelectSeparator />}
                      {gpuInstanceTypes.length > 0 && (
                        <SelectGroup>
                          <SelectLabel>GPU-Accelerated</SelectLabel>
                          {gpuInstanceTypes.map((type) => {
                            const gpu = gpuInfoByType.get(type)
                            return (
                              <SelectItem key={type} value={type}>
                                {type}
                                {gpu
                                  ? ` — ${formatVRAMMiB(gpu.totalMiB)}`
                                  : ""}{" "}
                                ({instanceTypeCounts[type]} available)
                              </SelectItem>
                            )
                          })}
                        </SelectGroup>
                      )}
                    </SelectContent>
                  </Select>
                )}
              />
              {selectedGPUInfo && (
                <FieldDescription>
                  GPU: {selectedGPUInfo.gpuName} ·{" "}
                  {formatVRAMMiB(selectedGPUInfo.totalMiB)} VRAM
                </FieldDescription>
              )}
              <FieldError errors={[errors.instanceType]} />
            </Field>

            {/* Key Pair */}
            <Field>
              <FieldTitle>
                <label htmlFor="keyName">Key Pair</label>
              </FieldTitle>
              <Controller
                control={control}
                name="keyName"
                render={({ field }) => (
                  <Select
                    onValueChange={(value) => field.onChange(value)}
                    value={field.value ?? ""}
                  >
                    <SelectTrigger
                      aria-invalid={!!errors.keyName}
                      className="w-full"
                      id="keyName"
                    >
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {keyPairs.map((keyPair) => (
                        <SelectItem
                          key={keyPair.KeyPairId}
                          value={keyPair.KeyName ?? ""}
                        >
                          {keyPair.KeyName}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
              <FieldError errors={[errors.keyName]} />
            </Field>

            {/* Subnet */}
            <Field>
              <FieldTitle>
                <label htmlFor="subnetId">Subnet</label>
              </FieldTitle>
              <Controller
                control={control}
                name="subnetId"
                render={({ field }) => (
                  <Select
                    onValueChange={(value) =>
                      field.onChange(value === "none" ? undefined : value)
                    }
                    value={field.value ?? "none"}
                  >
                    <SelectTrigger className="w-full" id="subnetId">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="none">none</SelectItem>
                      {subnets.map((subnet) => (
                        <SelectItem
                          key={subnet.SubnetId}
                          value={subnet.SubnetId ?? ""}
                        >
                          {subnet.SubnetId}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
            </Field>

            {/* Security Groups */}
            <Field>
              <FieldTitle>Security Group</FieldTitle>
              <FieldDescription>
                {effectiveVpcId
                  ? `Applies to VPC ${effectiveVpcId}`
                  : "No VPC resolved — select a subnet"}
              </FieldDescription>
              <Controller
                control={control}
                name="securityGroupMode"
                render={({ field }) => (
                  <div className="flex gap-4 text-sm">
                    <label className="flex items-center gap-2">
                      <input
                        aria-label="Create new security group"
                        checked={field.value === "create"}
                        onChange={() => field.onChange("create")}
                        type="radio"
                      />
                      Create new
                    </label>
                    <label className="flex items-center gap-2">
                      <input
                        aria-label="Select existing security group"
                        checked={field.value === "existing"}
                        onChange={() => field.onChange("existing")}
                        type="radio"
                      />
                      Select existing
                    </label>
                  </div>
                )}
              />

              {sgMode === "create" ? (
                <div className="mt-3 space-y-4 rounded-md border border-border p-4">
                  <Field>
                    <FieldTitle>
                      <label htmlFor="newSgName">Name</label>
                    </FieldTitle>
                    <Input
                      aria-invalid={!!errors.newSgName}
                      id="newSgName"
                      type="text"
                      {...register("newSgName")}
                    />
                    <FieldError errors={[errors.newSgName]} />
                  </Field>

                  <Field>
                    <FieldTitle>
                      <label htmlFor="newSgDescription">Description</label>
                    </FieldTitle>
                    <Input
                      id="newSgDescription"
                      type="text"
                      {...register("newSgDescription")}
                    />
                  </Field>

                  <Field>
                    <FieldTitle>Inbound rules</FieldTitle>
                    <div className="space-y-1">
                      <Controller
                        control={control}
                        name="allowSsh"
                        render={({ field }) => (
                          <label className="flex items-center gap-2 text-sm">
                            <input
                              aria-label="Allow SSH"
                              checked={field.value}
                              onChange={(e) => field.onChange(e.target.checked)}
                              type="checkbox"
                            />
                            Allow SSH (tcp/22)
                          </label>
                        )}
                      />
                      <Controller
                        control={control}
                        name="allowHttp"
                        render={({ field }) => (
                          <label className="flex items-center gap-2 text-sm">
                            <input
                              aria-label="Allow HTTP"
                              checked={field.value}
                              onChange={(e) => field.onChange(e.target.checked)}
                              type="checkbox"
                            />
                            Allow HTTP (tcp/80)
                          </label>
                        )}
                      />
                      <Controller
                        control={control}
                        name="allowHttps"
                        render={({ field }) => (
                          <label className="flex items-center gap-2 text-sm">
                            <input
                              aria-label="Allow HTTPS"
                              checked={field.value}
                              onChange={(e) => field.onChange(e.target.checked)}
                              type="checkbox"
                            />
                            Allow HTTPS (tcp/443)
                          </label>
                        )}
                      />
                    </div>
                  </Field>

                  <Field>
                    <FieldTitle>Source</FieldTitle>
                    <Controller
                      control={control}
                      name="ruleSource"
                      render={({ field }) => (
                        <div className="flex gap-4 text-sm">
                          <label className="flex items-center gap-2">
                            <input
                              aria-label="Source anywhere"
                              checked={field.value === "anywhere"}
                              onChange={() => field.onChange("anywhere")}
                              type="radio"
                            />
                            Anywhere (0.0.0.0/0)
                          </label>
                          <label className="flex items-center gap-2">
                            <input
                              aria-label="Source custom CIDR"
                              checked={field.value === "custom"}
                              onChange={() => field.onChange("custom")}
                              type="radio"
                            />
                            Custom CIDR
                          </label>
                        </div>
                      )}
                    />
                    {ruleSource === "custom" && (
                      <div className="mt-2">
                        <Input
                          aria-invalid={!!errors.customCidr}
                          placeholder="203.0.113.0/24"
                          type="text"
                          {...register("customCidr")}
                        />
                        <FieldError errors={[errors.customCidr]} />
                      </div>
                    )}
                  </Field>
                </div>
              ) : (
                <div className="mt-3">
                  {vpcSecurityGroups.length === 0 ? (
                    <p className="text-xs text-muted-foreground">
                      No security groups in the resolved VPC.
                    </p>
                  ) : (
                    <div className="space-y-1">
                      {vpcSecurityGroups.map((sg) => (
                        <label
                          className="flex items-center gap-2 text-xs"
                          key={sg.GroupId}
                        >
                          <input
                            aria-label={`Security group ${sg.GroupId} (${sg.GroupName})`}
                            checked={selectedSgIds.includes(sg.GroupId ?? "")}
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
                  <FieldError errors={[errors.securityGroupIds]} />
                </div>
              )}
            </Field>

            {/* Placement Group */}
            <Field>
              <FieldTitle>
                <label htmlFor="placementGroupName">Placement Group</label>
              </FieldTitle>
              <Controller
                control={control}
                name="placementGroupName"
                render={({ field }) => (
                  <Select
                    onValueChange={(value) =>
                      field.onChange(value === "none" ? undefined : value)
                    }
                    value={field.value ?? "none"}
                  >
                    <SelectTrigger className="w-full" id="placementGroupName">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="none">none</SelectItem>
                      {placementGroups.map((pg) => (
                        <SelectItem key={pg.GroupId} value={pg.GroupName ?? ""}>
                          {pg.GroupName} ({pg.Strategy})
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )}
              />
            </Field>
          </>
        )}

        {/* Instance Count */}
        <Field>
          <FieldTitle>
            <label htmlFor="count">Number of Instances</label>
          </FieldTitle>
          <Input
            aria-describedby="count-description"
            aria-invalid={!!errors.count}
            id="count"
            type="number"
            {...register("count", { valueAsNumber: true })}
          />
          <p className="text-xs text-muted-foreground" id="count-description">
            {selectedInstanceType &&
              `Available capacity for ${selectedInstanceType}: ${maxCount}`}
          </p>
          <FieldError errors={[errors.count]} />
        </Field>

        {!selectedTemplateId && (
          <>
            {/* Block Device Mappings (root volume) — collapsed by default */}
            <Collapsible>
              <CollapsibleTrigger
                className="group flex h-7 w-full items-center justify-between rounded-md border border-input bg-input/20 px-2 py-0.5 text-sm transition-colors outline-none hover:bg-input/40 focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30 md:text-xs/relaxed dark:bg-input/30 dark:hover:bg-input/50"
                render={
                  <button
                    aria-label="Block Device Mappings (root volume)"
                    type="button"
                  />
                }
              >
                <span>Block Device Mappings (root volume)</span>
                <ChevronDown className="size-3.5 text-muted-foreground transition-transform group-data-[panel-open]:rotate-180" />
              </CollapsibleTrigger>
              <CollapsibleContent className="mt-3 space-y-4 rounded-md border border-border p-4">
                <Field>
                  <FieldTitle>
                    <label htmlFor="rootDeviceName">Device Name</label>
                  </FieldTitle>
                  <Input
                    aria-invalid={!!errors.rootDeviceName}
                    id="rootDeviceName"
                    type="text"
                    {...register("rootDeviceName")}
                  />
                  <FieldError errors={[errors.rootDeviceName]} />
                </Field>

                <Field>
                  <FieldTitle>
                    <label htmlFor="rootVolumeSize">Volume Size (GiB)</label>
                  </FieldTitle>
                  <Input
                    aria-invalid={!!errors.rootVolumeSize}
                    id="rootVolumeSize"
                    max={MAX_VOLUME_SIZE_GIB}
                    min={selectedRoot.snapshotSize ?? MIN_VOLUME_SIZE_GIB}
                    placeholder={
                      selectedRoot.snapshotSize
                        ? `${selectedRoot.snapshotSize} (AMI default)`
                        : "use AMI / backend default"
                    }
                    type="number"
                    {...register("rootVolumeSize", {
                      setValueAs: (value: string) =>
                        value === "" || value === null || value === undefined
                          ? undefined
                          : Number(value),
                    })}
                  />
                  {selectedRoot.snapshotSize !== undefined && (
                    <FieldDescription>
                      AMI snapshot size: {selectedRoot.snapshotSize} GiB
                    </FieldDescription>
                  )}
                  <FieldError errors={[errors.rootVolumeSize]} />
                </Field>

                <Field>
                  <FieldTitle>
                    <label htmlFor="rootVolumeType">Volume Type</label>
                  </FieldTitle>
                  <Controller
                    control={control}
                    name="rootVolumeType"
                    render={({ field }) => (
                      <Select
                        onValueChange={(value) => field.onChange(value)}
                        value={field.value ?? DEFAULT_VOLUME_TYPE}
                      >
                        <SelectTrigger
                          aria-invalid={!!errors.rootVolumeType}
                          className="w-full"
                          id="rootVolumeType"
                        >
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          {VOLUME_TYPES.map((vt) => (
                            <SelectItem key={vt} value={vt}>
                              {vt}
                            </SelectItem>
                          ))}
                        </SelectContent>
                      </Select>
                    )}
                  />
                  <FieldError errors={[errors.rootVolumeType]} />
                </Field>

                <Field>
                  <Controller
                    control={control}
                    name="rootDeleteOnTermination"
                    render={({ field }) => (
                      <label className="flex items-center gap-2 text-sm">
                        <input
                          aria-label="Delete on termination"
                          checked={field.value ?? true}
                          onChange={(e) => field.onChange(e.target.checked)}
                          type="checkbox"
                        />
                        Delete on termination
                      </label>
                    )}
                  />
                </Field>
              </CollapsibleContent>
            </Collapsible>
          </>
        )}

        <CliCommandPanel
          commands={buildRunInstancesCommands(watch, effectiveVpcId)}
        />

        {/* Actions */}
        <div className="flex gap-2">
          <Button
            disabled={isSubmitting || createMutation.isPending}
            onClick={async () =>
              await navigate({ to: "/ec2/describe-instances" })
            }
            type="button"
            variant="outline"
          >
            Cancel
          </Button>
          <Button
            disabled={
              isSubmitting ||
              createMutation.isPending ||
              (!selectedTemplateId && uniqueInstanceTypes.length === 0)
            }
            type="submit"
          >
            {isSubmitting || createMutation.isPending
              ? "Creating…"
              : "Create Instance"}
          </Button>
        </div>
      </form>
    </>
  )
}

interface RootMappingDefaults {
  deviceName: string
  snapshotSize: number | undefined
}

interface ImageLike {
  RootDeviceName?: string
  BlockDeviceMappings?: { DeviceName?: string; Ebs?: { VolumeSize?: number } }[]
}

function getRootMapping(image: ImageLike | undefined): RootMappingDefaults {
  const deviceName = image?.RootDeviceName ?? DEFAULT_ROOT_DEVICE_NAME
  const mappings = image?.BlockDeviceMappings ?? []
  const root = mappings.find((m) => m.DeviceName === deviceName) ?? mappings[0]
  return {
    deviceName,
    snapshotSize: root?.Ebs?.VolumeSize,
  }
}

// resolveTemplateVersion maps a version selector ($Default, $Latest or a
// numbered version) onto the matching describe-versions entry.
function resolveTemplateVersion(
  versions: LaunchTemplateVersion[],
  selected: string | undefined,
): LaunchTemplateVersion | undefined {
  if (versions.length === 0) {
    return undefined
  }
  if (!selected || selected === "$Default") {
    return versions.find((v) => v.DefaultVersion) ?? versions[0]
  }
  if (selected === "$Latest") {
    return versions.toSorted(
      (a, b) => (b.VersionNumber ?? 0) - (a.VersionNumber ?? 0),
    )[0]
  }
  const num = Number(selected)
  return versions.find((v) => v.VersionNumber === num)
}

function buildRunInstancesCommands(
  watch: (name?: string) => unknown,
  vpcId: string | undefined,
): CliCommand[] {
  const rawLaunchTemplateId = watch("launchTemplateId")
  const launchTemplateId =
    typeof rawLaunchTemplateId === "string" ? rawLaunchTemplateId : ""
  if (launchTemplateId) {
    const rawTemplateVersion = watch("launchTemplateVersion")
    const templateVersion =
      typeof rawTemplateVersion === "string" && rawTemplateVersion
        ? rawTemplateVersion
        : "$Default"
    const rawTemplateCount = watch("count")
    const templateCount =
      typeof rawTemplateCount === "number" ? rawTemplateCount : 0
    return [
      {
        label: "Run Instances",
        parts: [
          {
            type: "bin" as const,
            value: "AWS_PROFILE=spinifex aws ec2 run-instances",
          },
          { type: "flag" as const, value: " \\\n  --launch-template" },
          {
            type: "value" as const,
            value: ` LaunchTemplateId=${launchTemplateId},Version=${templateVersion}`,
          },
          { type: "flag" as const, value: " \\\n  --count" },
          { type: "value" as const, value: ` ${templateCount || 1}` },
        ],
      },
    ]
  }

  const rawImageId = watch("imageId")
  const imageId = typeof rawImageId === "string" ? rawImageId : ""
  const rawInstanceType = watch("instanceType")
  const instanceType =
    typeof rawInstanceType === "string" ? rawInstanceType : ""
  const rawKeyName = watch("keyName")
  const keyName = typeof rawKeyName === "string" ? rawKeyName : ""
  const rawSubnetId = watch("subnetId")
  const subnetId = typeof rawSubnetId === "string" ? rawSubnetId : ""
  const rawPlacementGroupName = watch("placementGroupName")
  const placementGroupName =
    typeof rawPlacementGroupName === "string" ? rawPlacementGroupName : ""
  const rawCount = watch("count")
  const count = typeof rawCount === "number" ? rawCount : 0
  const rawRootDeviceName = watch("rootDeviceName")
  const rootDeviceName =
    typeof rawRootDeviceName === "string" ? rawRootDeviceName : ""
  const rawRootVolumeSize = watch("rootVolumeSize")
  const rootVolumeSize =
    typeof rawRootVolumeSize === "number" ? rawRootVolumeSize : undefined
  const rawRootVolumeType = watch("rootVolumeType")
  const rootVolumeType =
    typeof rawRootVolumeType === "string" ? rawRootVolumeType : ""
  const rawRootDeleteOnTermination = watch("rootDeleteOnTermination")
  const rootDeleteOnTermination =
    typeof rawRootDeleteOnTermination === "boolean"
      ? rawRootDeleteOnTermination
      : true

  const parts = [
    {
      type: "bin" as const,
      value: "AWS_PROFILE=spinifex aws ec2 run-instances",
    },
    { type: "flag" as const, value: " \\\n  --image-id" },
    { type: "value" as const, value: ` ${imageId || "<ImageId>"}` },
    { type: "flag" as const, value: " \\\n  --instance-type" },
    { type: "value" as const, value: ` ${instanceType || "<InstanceType>"}` },
    { type: "flag" as const, value: " \\\n  --key-name" },
    { type: "value" as const, value: ` ${keyName || "<KeyName>"}` },
  ]

  if (subnetId) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --subnet-id" },
      { type: "value" as const, value: ` ${subnetId}` },
    )
  }

  if (placementGroupName) {
    parts.push(
      { type: "flag" as const, value: " \\\n  --placement" },
      { type: "value" as const, value: ` GroupName=${placementGroupName}` },
    )
  }

  parts.push(
    { type: "flag" as const, value: " \\\n  --count" },
    { type: "value" as const, value: ` ${count || 1}` },
  )

  // Security groups — create-new emits create + authorize commands and feeds
  // $SG_ID into run-instances; select-existing passes the chosen IDs.
  const rawSgMode = watch("securityGroupMode")
  const sgMode = typeof rawSgMode === "string" ? rawSgMode : "create"
  const rawNewSgName = watch("newSgName")
  const sgName = typeof rawNewSgName === "string" ? rawNewSgName : ""
  const rawNewSgDesc = watch("newSgDescription")
  const sgDesc = typeof rawNewSgDesc === "string" ? rawNewSgDesc : ""
  const rawRuleSource = watch("ruleSource")
  const ruleSource =
    typeof rawRuleSource === "string" ? rawRuleSource : "anywhere"
  const rawCustomCidr = watch("customCidr")
  const customCidr = typeof rawCustomCidr === "string" ? rawCustomCidr : ""
  const sgCommands: CliCommand[] = []
  if (sgMode === "create") {
    sgCommands.push({
      label: "Create security group",
      parts: [
        {
          type: "bin" as const,
          value: "SG_ID=$(AWS_PROFILE=spinifex aws ec2 create-security-group",
        },
        { type: "flag" as const, value: " \\\n  --group-name" },
        { type: "value" as const, value: ` ${sgName || "<name>"}` },
        { type: "flag" as const, value: " \\\n  --description" },
        {
          type: "value" as const,
          value: ` '${sgDesc || "Created by launch wizard"}'`,
        },
        { type: "flag" as const, value: " \\\n  --vpc-id" },
        { type: "value" as const, value: ` ${vpcId ?? "<VpcId>"}` },
        { type: "flag" as const, value: " \\\n  --query" },
        { type: "value" as const, value: " 'GroupId' --output text)" },
      ],
    })

    const cidr =
      ruleSource === "custom" && customCidr ? customCidr : "0.0.0.0/0"
    const ports = [
      ...(watch("allowSsh") === true ? [22] : []),
      ...(watch("allowHttp") === true ? [80] : []),
      ...(watch("allowHttps") === true ? [443] : []),
    ]
    if (ports.length > 0) {
      const ipPerms = JSON.stringify(
        ports.map((port) => ({
          IpProtocol: "tcp",
          FromPort: port,
          ToPort: port,
          IpRanges: [{ CidrIp: cidr }],
        })),
      )
      sgCommands.push({
        label: "Authorize inbound rules",
        parts: [
          {
            type: "bin" as const,
            value:
              "AWS_PROFILE=spinifex aws ec2 authorize-security-group-ingress",
          },
          { type: "flag" as const, value: " \\\n  --group-id" },
          { type: "value" as const, value: " $SG_ID" },
          { type: "flag" as const, value: " \\\n  --ip-permissions" },
          { type: "value" as const, value: ` '${ipPerms}'` },
        ],
      })
    }
    parts.push(
      { type: "flag" as const, value: " \\\n  --security-group-ids" },
      { type: "value" as const, value: " $SG_ID" },
    )
  } else {
    const rawSgIds = watch("securityGroupIds")
    const sgIds = Array.isArray(rawSgIds)
      ? rawSgIds.filter((id): id is string => typeof id === "string")
      : []
    if (sgIds.length > 0) {
      parts.push(
        { type: "flag" as const, value: " \\\n  --security-group-ids" },
        { type: "value" as const, value: ` ${sgIds.join(" ")}` },
      )
    }
  }

  const hasStorageOverride =
    rootVolumeSize !== undefined ||
    rawRootVolumeType !== undefined ||
    rawRootDeleteOnTermination !== undefined
  if (hasStorageOverride) {
    const ebs: Record<string, unknown> = {}
    if (rootVolumeSize !== undefined) {
      ebs.VolumeSize = rootVolumeSize
    }
    if (rootVolumeType) {
      ebs.VolumeType = rootVolumeType
    }
    if (rawRootDeleteOnTermination !== undefined) {
      ebs.DeleteOnTermination = rootDeleteOnTermination
    }
    const bdm = JSON.stringify([
      {
        DeviceName: rootDeviceName || DEFAULT_ROOT_DEVICE_NAME,
        Ebs: ebs,
      },
    ])
    parts.push(
      { type: "flag" as const, value: " \\\n  --block-device-mappings" },
      { type: "value" as const, value: ` '${bdm}'` },
    )
  }

  return [...sgCommands, { label: "Run Instances", parts }]
}
