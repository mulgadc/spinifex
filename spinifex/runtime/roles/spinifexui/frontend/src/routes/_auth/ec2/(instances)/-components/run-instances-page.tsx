import type { LaunchTemplateVersion } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { ChevronDown } from "lucide-react"
import {
  Controller,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
  captureId,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  type CommandPart,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { GpuInstanceTypeSelect } from "@/components/gpu-instance-type-select"
import { PageHeading } from "@/components/page-heading"
import { SecurityGroupCheckboxes } from "@/components/security-group-checkboxes"
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
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
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

const DEFAULT_ROOT_DEVICE_NAME = "/dev/sda1"
const DEFAULT_VOLUME_TYPE = "gp3"
const MIN_VOLUME_SIZE_GIB = 1
const MAX_VOLUME_SIZE_GIB = 16_384

const INBOUND_RULE_CHECKBOXES = [
  { name: "allowSsh", label: "Allow SSH", text: "Allow SSH (tcp/22)" },
  { name: "allowHttp", label: "Allow HTTP", text: "Allow HTTP (tcp/80)" },
  { name: "allowHttps", label: "Allow HTTPS", text: "Allow HTTPS (tcp/443)" },
] as const

interface RunInstancesPageProps {
  launchTemplateId?: string
  launchTemplateVersion?: string
  // Given when the form sits in the list's side panel, which owns the title
  // and is closed rather than navigated away from.
  onClose?: () => void
}

export function RunInstancesPage({
  launchTemplateId,
  launchTemplateVersion,
  onClose,
}: RunInstancesPageProps) {
  const navigate = useNavigate()

  async function leave() {
    if (onClose) {
      onClose()

      return
    }

    await navigate({ to: "/ec2/describe-instances" })
  }

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

  for (const type of instanceTypesData.InstanceTypes ?? []) {
    const typeName = type.InstanceType

    if (!typeName) {
      continue
    }

    instanceTypeCounts[typeName] = (instanceTypeCounts[typeName] ?? 0) + 1
  }

  const uniqueInstanceTypes = Object.keys(instanceTypeCounts).toSorted()

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

  const selectedTypeInfo = instanceTypesData.InstanceTypes?.find(
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

  const effectiveVpcId =
    subnets.find((s) => s.SubnetId === selectedSubnetId)?.VpcId ?? defaultVpcId

  const vpcSecurityGroups = effectiveVpcId
    ? securityGroups.filter((sg) => sg.VpcId === effectiveVpcId)
    : securityGroups

  const onSubmit = async (data: CreateInstanceFormData) => {
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

    await createMutation.mutateAsync(params)
    await leave()
  }

  return (
    <>
      {!onClose && (
        <>
          <BackLink to="/ec2/describe-instances">Back to instances</BackLink>
          <PageHeading title="Run Instances" />
        </>
      )}

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
                      onValueChange={(value) => {
                        field.onChange(value)
                        // Re-prefill DeviceName for the new AMI. Size, type and
                        // delete-on-termination stay blank so an unchanged form
                        // sends no BlockDeviceMappings.
                        setValue(
                          "rootDeviceName",
                          getRootMapping(
                            images.find((img) => img.ImageId === value),
                          ).deviceName,
                        )
                      }}
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
                  <GpuInstanceTypeSelect
                    aria-invalid={!!errors.instanceType}
                    availabilityByType={instanceTypeCounts}
                    className="w-full"
                    id="instanceType"
                    instanceTypes={instanceTypesData.InstanceTypes ?? []}
                    onValueChange={field.onChange}
                    value={field.value ?? ""}
                  />
                )}
              />
              {selectedGpuDevice && (
                <FieldDescription>
                  GPU: {selectedGpuDevice.Name}
                  {selectedGpuDevice.MemoryInfo?.SizeInMiB !== undefined &&
                    ` · ${formatVRAMMiB(selectedGpuDevice.MemoryInfo.SizeInMiB)} VRAM`}
                  {selectedGpuMigProfile &&
                    ` · MIG ${selectedGpuMigProfile} slice`}
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
                    onValueChange={(value) => {
                      field.onChange(value === "none" ? undefined : value)
                    }}
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
                      {INBOUND_RULE_CHECKBOXES.map(({ name, label, text }) => (
                        <label
                          className="flex items-center gap-2 text-sm"
                          key={name}
                        >
                          <input
                            aria-label={label}
                            type="checkbox"
                            {...register(name)}
                          />
                          {text}
                        </label>
                      ))}
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
                  <SecurityGroupCheckboxes
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
                    onValueChange={(value) => {
                      field.onChange(value === "none" ? undefined : value)
                    }}
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
                className="group"
                variant="field"
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
              <CollapsibleContent className="mt-3" variant="panel">
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

                <Field>
                  <Controller
                    control={control}
                    name="rootDeleteOnTermination"
                    render={({ field }) => (
                      <label className="flex items-center gap-2 text-sm">
                        <input
                          aria-label="Delete on termination"
                          checked={field.value ?? true}
                          onChange={(e) => {
                            field.onChange(e.target.checked)
                          }}
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
          commands={buildRunInstancesCommands(values, effectiveVpcId)}
        />

        {/* Actions */}
        <div className="flex gap-2">
          <Button
            disabled={isSubmitting || createMutation.isPending}
            onClick={leave}
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
