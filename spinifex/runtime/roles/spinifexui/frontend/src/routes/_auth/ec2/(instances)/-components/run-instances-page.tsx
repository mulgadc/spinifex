import type {
  InstanceTypeInfo,
  LaunchTemplateVersion,
  Subnet,
} from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Cpu } from "lucide-react"
import { useState } from "react"
import {
  Controller,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  captureId,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  type CommandPart,
} from "@/components/cli-command-panel"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { GpuInstanceTypeSelect } from "@/components/gpu-instance-type-select"
import { InfoTip } from "@/components/info-tip"
import { SecurityGroupPicker } from "@/components/security-group-picker"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
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
import { getRegion } from "@/lib/cluster-config"
import {
  type Architecture,
  buildImageCatalog,
  imageArchitecture,
  imageArchitectures,
  instanceTypeSupports,
  latestOfDistro,
  parseImageName,
  preferredImage,
} from "@/lib/os-images"
import { isSystemManagedImage } from "@/lib/system-managed"
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

import { ChoiceTile, ChoiceTiles } from "./choice-tile"
import { ImagePicker } from "./image-picker"
import { LocationTile } from "./location-tile"

const DEFAULT_ROOT_DEVICE_NAME = "/dev/sda1"
const DEFAULT_VOLUME_TYPE = "gp3"
const MIN_VOLUME_SIZE_GIB = 1
const MAX_VOLUME_SIZE_GIB = 16_384
const DEFAULT_SUBNET = "default"

const INBOUND_RULE_SWITCHES = [
  { name: "allowSsh", label: "Allow SSH", text: "SSH (tcp/22)" },
  { name: "allowHttp", label: "Allow HTTP", text: "HTTP (tcp/80)" },
  { name: "allowHttps", label: "Allow HTTPS", text: "HTTPS (tcp/443)" },
] as const

interface RunInstancesPageProps {
  launchTemplateId?: string
  launchTemplateVersion?: string
  // Called with the new instance's ID when one was launched, or with nothing
  // on cancel or for several.
  onClose: (instanceId?: string) => void
}

// "2 vCPU · 8 GiB memory", read from DescribeInstanceTypes, which reports
// the size of every type the cluster offers.
export function describeTypeSize(type: InstanceTypeInfo): string {
  const vcpus = type.VCpuInfo?.DefaultVCpus
  const mib = type.MemoryInfo?.SizeInMiB

  return [
    vcpus === undefined ? undefined : `${vcpus} vCPU`,
    mib === undefined
      ? undefined
      : `${Number((mib / 1024).toFixed(2))} GiB memory`,
  ]
    .filter(Boolean)
    .join(" · ")
}

export function RunInstancesPage({
  launchTemplateId,
  launchTemplateVersion,
  onClose,
}: RunInstancesPageProps) {
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

  const images = (imagesData.Images ?? []).filter(
    (image) => !isSystemManagedImage(image),
  )

  const allInstanceTypes = instanceTypesData.InstanceTypes ?? []
  const keyPairs = keyPairsData.KeyPairs ?? []
  const subnets = subnetsData.Subnets ?? []
  const placementGroups = pgData.PlacementGroups ?? []
  const vpcs = vpcsData.Vpcs ?? []
  const securityGroups = sgData.SecurityGroups ?? []
  const launchTemplates = ltData.LaunchTemplates ?? []
  const defaultVpcId = (vpcs.find((v) => v.IsDefault) ?? vpcs[0])?.VpcId

  // RunInstances with no subnet lands in the account's default subnet, which
  // DescribeSubnets marks DefaultForAz.
  const defaultSubnet = subnets.find((s) => s.DefaultForAz)
  const otherSubnets = subnets.filter((s) => s !== defaultSubnet)

  const defaultSgName = nextLaunchWizardName(
    securityGroups.map((sg) => sg.GroupName ?? ""),
  )

  const instanceTypeCounts: Record<string, number> = {}

  for (const type of allInstanceTypes) {
    const typeName = type.InstanceType

    if (!typeName) {
      continue
    }

    instanceTypeCounts[typeName] = (instanceTypeCounts[typeName] ?? 0) + 1
  }

  const hasCompute = Object.keys(instanceTypeCounts).length > 0

  // The architecture choice appears only when the catalog holds both; it
  // narrows the images and the instance types together, so a launch cannot
  // pair an arm64 type with an x86_64 image.
  const architectures = imageArchitectures(images)

  const initialArch =
    architectures.find((candidate) =>
      allInstanceTypes.some((type) => instanceTypeSupports(type, candidate)),
    ) ?? architectures[0]

  const [arch, setArch] = useState<Architecture | undefined>(initialArch)
  const catalog = buildImageCatalog(images, arch)
  const archTypes = typesForArchitecture(allInstanceTypes, arch)
  const archTypeNames = uniqueTypeNames(archTypes)

  const defaultImageId = preferredImage(catalog)?.ImageId
  const defaultKeyName = keyPairs[0]?.KeyName
  const defaultInstanceType = defaultTypeName(archTypeNames)

  const defaultRoot = getRootMapping(
    images.find((img) => img.ImageId === defaultImageId),
  )

  const {
    control,
    handleSubmit,
    register,
    setValue,
    getValues,
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
          const image = images.find((img) => img.ImageId === data.imageId)
          const minSize = getRootMapping(image).snapshotSize

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

          const imageArch = image && imageArchitecture(image)

          const type = allInstanceTypes.find(
            (t) => t.InstanceType === data.instanceType,
          )

          if (
            !data.launchTemplateId &&
            type &&
            imageArch &&
            !instanceTypeSupports(type, imageArch)
          ) {
            ctx.addIssue({
              code: "custom",
              message: `${data.instanceType} cannot run a ${imageArch} image`,
              path: ["instanceType"],
            })
          }
        }),
    ),
    defaultValues: {
      count: 1,
      imageId: defaultImageId ?? "",
      keyName: defaultKeyName ?? "",
      instanceType: defaultInstanceType ?? "",
      launchTemplateId,
      launchTemplateVersion: launchTemplateVersion ?? "$Default",
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

  const values = useWatch({ control })
  const selectedInstanceType = useWatch({ control, name: "instanceType" })

  const maxCount = selectedInstanceType
    ? (instanceTypeCounts[selectedInstanceType] ?? 1)
    : 1

  const selectedTypeInfo = allInstanceTypes.find(
    (t) => t.InstanceType === selectedInstanceType,
  )

  const selectedGpuDevice = selectedTypeInfo?.GpuInfo?.Gpus?.[0]

  const selectedGpuMigProfile = selectedGpuDevice?.Name?.startsWith("MIG ")
    ? selectedGpuDevice.Name.slice("MIG ".length)
    : undefined

  const selectedImageId = useWatch({ control, name: "imageId" })

  const selectedRoot = getRootMapping(
    images.find((img) => img.ImageId === selectedImageId),
  )

  const selectedSubnetId = useWatch({ control, name: "subnetId" })
  const sgMode = useWatch({ control, name: "securityGroupMode" })
  const ruleSource = useWatch({ control, name: "ruleSource" })
  const selectedSgIds = useWatch({ control, name: "securityGroupIds" }) ?? []

  // Launch template: when one is selected the instance is launched from it and
  // the direct-config fields below are hidden — the template supplies them.
  const selectedTemplateId = useWatch({ control, name: "launchTemplateId" })

  const selectedTemplateVersion = useWatch({
    control,
    name: "launchTemplateVersion",
  })

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

  const effectiveSubnet = selectedSubnetId
    ? subnets.find((s) => s.SubnetId === selectedSubnetId)
    : defaultSubnet

  const effectiveVpcId = effectiveSubnet?.VpcId ?? defaultVpcId

  const vpcSecurityGroups = effectiveVpcId
    ? securityGroups.filter((sg) => sg.VpcId === effectiveVpcId)
    : securityGroups

  function selectImage(imageId: string) {
    setValue("imageId", imageId, { shouldValidate: !!errors.imageId })
    // Re-prefill DeviceName for the new AMI. Size, type and
    // delete-on-termination stay blank so an unchanged form
    // sends no BlockDeviceMappings.
    setValue(
      "rootDeviceName",
      getRootMapping(images.find((img) => img.ImageId === imageId)).deviceName,
    )
  }

  // Changing architecture keeps the chosen distro where the other
  // architecture has it, and swaps any type that cannot run the new images.
  function selectArchitecture(next: Architecture) {
    setArch(next)

    const nextCatalog = buildImageCatalog(images, next)
    const current = images.find((img) => img.ImageId === getValues("imageId"))
    const currentArch = current && imageArchitecture(current)

    if (!current || (currentArch && currentArch !== next)) {
      const replacement =
        latestOfDistro(nextCatalog, parseImageName(current?.Name)?.distro) ??
        preferredImage(nextCatalog)

      selectImage(replacement?.ImageId ?? "")
    }

    const type = allInstanceTypes.find(
      (t) => t.InstanceType === getValues("instanceType"),
    )

    if (!type || !instanceTypeSupports(type, next)) {
      setValue(
        "instanceType",
        defaultTypeName(
          uniqueTypeNames(typesForArchitecture(allInstanceTypes, next)),
        ) ?? "",
      )
    }
  }

  const onSubmit = async (data: CreateInstanceFormData) => {
    // The banner above says why; without compute only a template can launch.
    if (!data.launchTemplateId && !hasCompute) {
      return
    }

    // Launch wholly from the template (plus count). Build params from only the
    // fields that apply so no direct-config field — including hidden ones left
    // in form state — overrides what the template defines.
    const params: CreateInstanceParams = data.launchTemplateId
      ? {
          count: data.count,
          launchTemplateId: data.launchTemplateId,
          launchTemplateVersion: data.launchTemplateVersion,
          resolvedVpcId: effectiveVpcId,
        }
      : { ...data, resolvedVpcId: effectiveVpcId }

    let result

    try {
      result = await createMutation.mutateAsync(params)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    const launched = result.Instances ?? []

    onClose(launched.length === 1 ? launched[0]?.InstanceId : undefined)
  }

  const showTemplates = launchTemplates.length > 0 || !!launchTemplateId

  const countField = (
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
  )

  return (
    <>
      {!hasCompute && !selectedTemplateId && (
        <ErrorBanner msg="No compute available. No new instances can be created until compute is available." />
      )}

      <form onSubmit={handleSubmit(onSubmit)}>
        <FormLock
          className="flex flex-col gap-4"
          locked={isSubmitting || createMutation.isPending}
        >
          {showTemplates && (
            <PanelSection title="Launch template">
              <div className="flex flex-col gap-4">
                <Field>
                  <FieldTitle>
                    <label htmlFor="launchTemplateId">Launch Template</label>
                    <InfoTip label="Launch template">
                      A template supplies the image, instance type, key pair and
                      network settings, so those fields are hidden while one is
                      chosen.
                    </InfoTip>
                  </FieldTitle>
                  <Controller
                    control={control}
                    name="launchTemplateId"
                    render={({ field }) => (
                      <Select
                        onValueChange={(value) => {
                          field.onChange(value === "none" ? undefined : value)
                        }}
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
                          onValueChange={field.onChange}
                          value={field.value ?? "$Default"}
                        >
                          <SelectTrigger
                            className="w-full"
                            id="launchTemplateVersion"
                          >
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

                {selectedTemplateId && countField}
              </div>
            </PanelSection>
          )}

          {!selectedTemplateId && (
            <>
              <PanelSection title="Image">
                <div className="flex flex-col gap-4">
                  {architectures.length > 1 && (
                    <Field>
                      <FieldTitle>
                        Architecture
                        <InfoTip label="Architecture">
                          An instance type runs one architecture, so choosing
                          one narrows both the images and the instance types to
                          those that can run together.
                        </InfoTip>
                      </FieldTitle>
                      <ChoiceTiles columns={2} label="Architecture">
                        {architectures.map((candidate) => (
                          <ChoiceTile
                            checked={arch === candidate}
                            description={
                              candidate === "arm64"
                                ? "64-bit Arm"
                                : "64-bit x86"
                            }
                            icon={
                              <Cpu
                                aria-hidden
                                className="size-5 text-muted-foreground"
                              />
                            }
                            key={candidate}
                            label={candidate}
                            layout="inline"
                            onSelect={() => {
                              selectArchitecture(candidate)
                            }}
                          />
                        ))}
                      </ChoiceTiles>
                    </Field>
                  )}

                  <Field>
                    <FieldTitle>Operating system</FieldTitle>
                    <ImagePicker
                      arch={arch}
                      images={images}
                      invalid={!!errors.imageId}
                      onChange={selectImage}
                      value={selectedImageId ?? ""}
                    />
                    <FieldError errors={[errors.imageId]} />
                  </Field>
                </div>
              </PanelSection>

              <PanelSection title="Instance">
                <div className="flex flex-col gap-4">
                  <Field>
                    <FieldTitle>
                      <label htmlFor="instanceType">Instance Type</label>
                      <InfoTip label="Instance type">
                        vCPUs and memory for the instance, with the free
                        capacity for each type. Only types that can run the
                        chosen image&apos;s architecture are listed.
                      </InfoTip>
                    </FieldTitle>
                    <Controller
                      control={control}
                      name="instanceType"
                      render={({ field }) => (
                        <GpuInstanceTypeSelect
                          aria-invalid={!!errors.instanceType}
                          availabilityByType={instanceTypeCounts}
                          className="w-full"
                          id="instanceType"
                          instanceTypes={archTypes}
                          onValueChange={field.onChange}
                          value={field.value ?? ""}
                        />
                      )}
                    />
                    {hasCompute && archTypeNames.length === 0 && arch && (
                      <FieldDescription>
                        No instance types on this cluster run {arch}.
                      </FieldDescription>
                    )}
                    {selectedTypeInfo && (
                      <FieldDescription data-testid="instance-type-size">
                        {describeTypeSize(selectedTypeInfo)}
                      </FieldDescription>
                    )}
                    {selectedGpuDevice && (
                      <FieldDescription>
                        GPU: {selectedGpuDevice.Name}
                        {selectedGpuDevice.MemoryInfo?.SizeInMiB !==
                          undefined &&
                          ` · ${formatVRAMMiB(selectedGpuDevice.MemoryInfo.SizeInMiB)} VRAM`}
                        {selectedGpuMigProfile &&
                          ` · MIG ${selectedGpuMigProfile} slice`}
                      </FieldDescription>
                    )}
                    <FieldError errors={[errors.instanceType]} />
                  </Field>

                  {countField}

                  <Field>
                    <FieldTitle>
                      <label htmlFor="keyName">Key Pair</label>
                    </FieldTitle>
                    <Controller
                      control={control}
                      name="keyName"
                      render={({ field }) => (
                        <Select
                          onValueChange={field.onChange}
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

                  <Field>
                    <FieldTitle>
                      <label htmlFor="placementGroupName">
                        Placement Group
                      </label>
                    </FieldTitle>
                    <Controller
                      control={control}
                      name="placementGroupName"
                      render={({ field }) => (
                        <Select
                          onValueChange={(value) => {
                            field.onChange(value === "none" ? undefined : value)
                          }}
                          value={field.value ?? "none"}
                        >
                          <SelectTrigger
                            className="w-full"
                            id="placementGroupName"
                          >
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value="none">none</SelectItem>
                            {placementGroups.map((pg) => (
                              <SelectItem
                                key={pg.GroupId}
                                value={pg.GroupName ?? ""}
                              >
                                {pg.GroupName} ({pg.Strategy})
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      )}
                    />
                  </Field>
                </div>
              </PanelSection>

              <PanelSection title="Network">
                <div className="flex flex-col gap-4">
                  <Field>
                    <FieldTitle>
                      Location
                      <InfoTip label="Location">
                        The region this cluster serves. The availability zone
                        comes from the subnet the instance launches into.
                      </InfoTip>
                    </FieldTitle>
                    <LocationTile
                      availabilityZone={effectiveSubnet?.AvailabilityZone}
                      region={getRegion()}
                    />
                  </Field>

                  <Field>
                    <FieldTitle>
                      <label htmlFor="subnetId">Subnet</label>
                      <InfoTip label="Subnet">
                        With no subnet chosen, EC2 places the instance in the
                        default VPC&apos;s default subnet. It gets a private
                        address there, and other instances in the VPC can reach
                        it as its security groups allow. It gets a public
                        address only if the subnet assigns one on launch.
                      </InfoTip>
                    </FieldTitle>
                    <Controller
                      control={control}
                      name="subnetId"
                      render={({ field }) => (
                        <Select
                          onValueChange={(value) => {
                            field.onChange(
                              value === DEFAULT_SUBNET ? undefined : value,
                            )
                          }}
                          value={field.value ?? DEFAULT_SUBNET}
                        >
                          <SelectTrigger className="w-full" id="subnetId">
                            <SelectValue>
                              {effectiveSubnet
                                ? `${selectedSubnetId ? "" : "Default: "}${subnetLabel(effectiveSubnet)}`
                                : "No subnet"}
                            </SelectValue>
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value={DEFAULT_SUBNET}>
                              {defaultSubnet
                                ? `Default: ${subnetLabel(defaultSubnet)}`
                                : "No subnet"}
                            </SelectItem>
                            {otherSubnets.map((subnet) => (
                              <SelectItem
                                key={subnet.SubnetId}
                                value={subnet.SubnetId ?? ""}
                              >
                                {subnetLabel(subnet)}
                              </SelectItem>
                            ))}
                          </SelectContent>
                        </Select>
                      )}
                    />
                    <FieldDescription>
                      {effectiveSubnet
                        ? subnetSummary(effectiveSubnet)
                        : "This account has no default subnet, so an instance launched without one gets no VPC network interface. Choose a subnet."}
                    </FieldDescription>
                  </Field>

                  <Field>
                    <FieldTitle>
                      Security Group
                      <InfoTip label="Security group">
                        A new group allows only the inbound ports switched on
                        below. Outbound traffic is allowed either way.
                      </InfoTip>
                    </FieldTitle>
                    <FieldDescription>
                      {effectiveVpcId
                        ? `Applies to VPC ${effectiveVpcId}`
                        : "No VPC resolved — select a subnet"}
                    </FieldDescription>
                    <div className="flex gap-4 text-sm">
                      <label className="flex items-center gap-2">
                        <input
                          aria-label="Create new security group"
                          type="radio"
                          value="create"
                          {...register("securityGroupMode")}
                        />
                        Create new
                      </label>
                      <label className="flex items-center gap-2">
                        <input
                          aria-label="Select existing security group"
                          type="radio"
                          value="existing"
                          {...register("securityGroupMode")}
                        />
                        Select existing
                      </label>
                    </div>

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
                            <label htmlFor="newSgDescription">
                              Description
                            </label>
                          </FieldTitle>
                          <Input
                            id="newSgDescription"
                            type="text"
                            {...register("newSgDescription")}
                          />
                        </Field>

                        <Field>
                          <FieldTitle>Inbound rules</FieldTitle>
                          <div className="flex flex-col gap-2">
                            {INBOUND_RULE_SWITCHES.map(
                              ({ name, label, text }) => (
                                <Controller
                                  control={control}
                                  key={name}
                                  name={name}
                                  render={({ field }) => (
                                    <div className="flex items-center gap-2">
                                      <Switch
                                        aria-label={label}
                                        checked={field.value ?? false}
                                        id={name}
                                        onCheckedChange={field.onChange}
                                      />
                                      <Label htmlFor={name}>{text}</Label>
                                    </div>
                                  )}
                                />
                              ),
                            )}
                          </div>
                        </Field>

                        <Field>
                          <FieldTitle>Source</FieldTitle>
                          <div className="flex gap-4 text-sm">
                            <label className="flex items-center gap-2">
                              <input
                                aria-label="Source anywhere"
                                type="radio"
                                value="anywhere"
                                {...register("ruleSource")}
                              />
                              Anywhere (0.0.0.0/0)
                            </label>
                            <label className="flex items-center gap-2">
                              <input
                                aria-label="Source custom CIDR"
                                type="radio"
                                value="custom"
                                {...register("ruleSource")}
                              />
                              Custom CIDR
                            </label>
                          </div>
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
                        <SecurityGroupPicker
                          emptyText="No security groups in the resolved VPC."
                          groups={vpcSecurityGroups}
                          onChange={(next) => {
                            setValue("securityGroupIds", next, {
                              shouldValidate: true,
                            })
                          }}
                          selected={selectedSgIds}
                        />
                        <FieldError errors={[errors.securityGroupIds]} />
                      </div>
                    )}
                  </Field>
                </div>
              </PanelSection>

              <PanelSection title="Root volume">
                <div className="flex flex-col gap-4">
                  <div className="grid gap-4 sm:grid-cols-2">
                    <Field>
                      <FieldTitle>
                        <label htmlFor="rootVolumeSize">Size (GiB)</label>
                        <InfoTip label="Root volume size">
                          Leave blank to use the image&apos;s own size. A larger
                          size grows the root volume; it cannot be smaller than
                          the image&apos;s snapshot.
                        </InfoTip>
                      </FieldTitle>
                      <Input
                        aria-invalid={!!errors.rootVolumeSize}
                        id="rootVolumeSize"
                        max={MAX_VOLUME_SIZE_GIB}
                        min={selectedRoot.snapshotSize ?? MIN_VOLUME_SIZE_GIB}
                        placeholder={
                          selectedRoot.snapshotSize
                            ? String(selectedRoot.snapshotSize)
                            : "Image default"
                        }
                        type="number"
                        {...register("rootVolumeSize", {
                          setValueAs: (value: string) =>
                            value === "" ||
                            value === null ||
                            value === undefined
                              ? undefined
                              : Number(value),
                        })}
                      />
                      <FieldDescription>
                        {selectedRoot.snapshotSize === undefined
                          ? "The image does not report a size."
                          : `Minimum ${selectedRoot.snapshotSize} GiB, the image's snapshot size.`}
                      </FieldDescription>
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
                            onValueChange={field.onChange}
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
                  </div>

                  <Field>
                    <FieldTitle>
                      <label htmlFor="rootDeviceName">Device Name</label>
                    </FieldTitle>
                    <Input
                      aria-invalid={!!errors.rootDeviceName}
                      id="rootDeviceName"
                      type="text"
                      variant="code"
                      {...register("rootDeviceName")}
                    />
                    <FieldError errors={[errors.rootDeviceName]} />
                  </Field>

                  <Controller
                    control={control}
                    name="rootDeleteOnTermination"
                    render={({ field }) => (
                      <div className="flex items-center gap-2">
                        <Switch
                          aria-label="Delete on termination"
                          checked={field.value ?? true}
                          id="rootDeleteOnTermination"
                          onCheckedChange={field.onChange}
                        />
                        <Label htmlFor="rootDeleteOnTermination">
                          Delete on termination
                        </Label>
                        <InfoTip label="Delete on termination">
                          When on, the root volume is deleted with the instance.
                          Turn it off to keep the volume after termination.
                        </InfoTip>
                      </div>
                    )}
                  />
                </div>
              </PanelSection>
            </>
          )}

          <CliCommandPanel
            commands={buildRunInstancesCommands(values, effectiveVpcId)}
          />

          <FormActions
            error={createMutation.error}
            errorMsg="Failed to create instance"
            isPending={createMutation.isPending}
            isSubmitting={isSubmitting}
            onCancel={() => {
              onClose()
            }}
            pendingLabel="Creating…"
            submitDisabled={!hasCompute && !selectedTemplateId}
            submitLabel="Create Instance"
          />
        </FormLock>
      </form>
    </>
  )
}

function typesForArchitecture(
  types: readonly InstanceTypeInfo[],
  arch: Architecture | undefined,
): InstanceTypeInfo[] {
  return types.filter((type) => instanceTypeSupports(type, arch))
}

function uniqueTypeNames(types: readonly InstanceTypeInfo[]): string[] {
  return [
    ...new Set(types.flatMap((type) => type.InstanceType ?? [])),
  ].toSorted()
}

function defaultTypeName(names: readonly string[]): string | undefined {
  return names.find((name) => name.endsWith(".nano")) ?? names[0]
}

function subnetName(subnet: Subnet): string | undefined {
  return subnet.Tags?.find((tag) => tag.Key === "Name")?.Value
}

function subnetLabel(subnet: Subnet): string {
  const name = subnetName(subnet)

  return name ? `${name} (${subnet.SubnetId})` : (subnet.SubnetId ?? "")
}

function subnetSummary(subnet: Subnet): string {
  return [
    subnet.CidrBlock,
    subnet.AvailabilityZone,
    subnet.VpcId,
    subnet.MapPublicIpOnLaunch
      ? "public IP assigned on launch"
      : "private address only",
  ]
    .filter(Boolean)
    .join(" · ")
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
  values: DeepPartialSkipArrayKey<CreateInstanceFormData>,
  vpcId: string | undefined,
): CliCommand[] {
  const bin: CommandPart = {
    type: "bin",
    value: "AWS_PROFILE=spinifex aws ec2 run-instances",
  }

  if (values.launchTemplateId) {
    const templateVersion = values.launchTemplateVersion || "$Default"

    return [
      {
        label: "Run Instances",
        parts: [
          bin,
          ...commandFlag(
            "\\\n  --launch-template",
            `LaunchTemplateId=${values.launchTemplateId},Version=${templateVersion}`,
          ),
          ...commandFlag("\\\n  --count", (values.count ?? 0) || 1),
        ],
      },
    ]
  }

  const parts: CommandPart[] = [
    bin,
    ...commandFlag(
      "\\\n  --image-id",
      cliPlaceholder(values.imageId ?? "", "ImageId"),
    ),
    ...commandFlag(
      "\\\n  --instance-type",
      cliPlaceholder(values.instanceType ?? "", "InstanceType"),
    ),
    ...commandFlag(
      "\\\n  --key-name",
      cliPlaceholder(values.keyName ?? "", "KeyName"),
    ),
    ...optionalFlag("\\\n  --subnet-id", values.subnetId),
    ...optionalFlag(
      "\\\n  --placement",
      values.placementGroupName && `GroupName=${values.placementGroupName}`,
    ),
    ...commandFlag("\\\n  --count", (values.count ?? 0) || 1),
  ]

  // Security groups — create-new emits create + authorize commands and feeds
  // $SG_ID into run-instances; select-existing passes the chosen IDs.
  const sgCommands: CliCommand[] = []

  if ((values.securityGroupMode || "create") === "create") {
    sgCommands.push({
      label: "Create security group",
      parts: captureId(
        "SG_ID",
        "AWS_PROFILE=spinifex aws ec2 create-security-group",
        [
          ...commandFlag(
            "\\\n  --group-name",
            cliPlaceholder(values.newSgName ?? "", "name"),
          ),
          ...commandFlag(
            "\\\n  --description",
            `'${values.newSgDescription || "Created by launch wizard"}'`,
          ),
          ...commandFlag("\\\n  --vpc-id", vpcId ?? "<VpcId>"),
        ],
        "GroupId",
      ),
    })

    const cidr =
      values.ruleSource === "custom" && values.customCidr
        ? values.customCidr
        : "0.0.0.0/0"

    const ports = [
      ...(values.allowSsh === true ? [22] : []),
      ...(values.allowHttp === true ? [80] : []),
      ...(values.allowHttps === true ? [443] : []),
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
            type: "bin",
            value:
              "AWS_PROFILE=spinifex aws ec2 authorize-security-group-ingress",
          },
          ...commandFlag("\\\n  --group-id", "$SG_ID"),
          ...commandFlag("\\\n  --ip-permissions", `'${ipPerms}'`),
        ],
      })
    }

    parts.push(...commandFlag("\\\n  --security-group-ids", "$SG_ID"))
  } else {
    parts.push(
      ...optionalFlag(
        "\\\n  --security-group-ids",
        values.securityGroupIds?.join(" "),
      ),
    )
  }

  const { rootDeleteOnTermination, rootVolumeSize, rootVolumeType } = values

  const hasStorageOverride =
    rootVolumeSize !== undefined ||
    rootVolumeType !== undefined ||
    rootDeleteOnTermination !== undefined

  if (hasStorageOverride) {
    // A field the form left unset stays undefined, which JSON.stringify omits,
    // so the CLI preview shows only the overrides the user actually set.
    const ebs = {
      VolumeSize: rootVolumeSize,
      VolumeType: rootVolumeType || undefined,
      DeleteOnTermination: rootDeleteOnTermination,
    }

    const bdm = JSON.stringify([
      {
        DeviceName: values.rootDeviceName || DEFAULT_ROOT_DEVICE_NAME,
        Ebs: ebs,
      },
    ])

    parts.push(...commandFlag("\\\n  --block-device-mappings", `'${bdm}'`))
  }

  return [...sgCommands, { label: "Run Instances", parts }]
}
