import type {
  GpuDeviceInfo,
  Image,
  InstanceTypeInfo,
  KeyPairInfo,
  LaunchTemplate,
  PlacementGroup,
  SecurityGroup,
  Subnet,
} from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Cpu } from "lucide-react"
import type { ReactNode } from "react"
import {
  type Control,
  Controller,
  type FieldErrors,
  type UseFormRegister,
  type UseFormSetValue,
  useWatch,
} from "react-hook-form"

import { PanelSection } from "@/components/detail-panel"
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
import type { Architecture } from "@/lib/os-images"
import { formatVRAMMiB } from "@/lib/utils"
import { ec2LaunchTemplateVersionsQueryOptions } from "@/queries/ec2"
import { type CreateInstanceFormData, VOLUME_TYPES } from "@/types/ec2"

import { ChoiceTile, ChoiceTiles } from "./choice-tile"
import { ImagePicker } from "./image-picker"
import { LocationTile } from "./location-tile"
import {
  describeTypeSize,
  imageRootMapping,
  resolveTemplateVersion,
  type RunInstancesFormInput,
  subnetLabel,
  subnetSummary,
} from "./run-instances-helpers"

const DEFAULT_VOLUME_TYPE = "gp3"
const MIN_VOLUME_SIZE_GIB = 1
const MAX_VOLUME_SIZE_GIB = 16_384
const DEFAULT_SUBNET = "default"

const INBOUND_RULE_SWITCHES = [
  { name: "allowSsh", label: "Allow SSH", text: "SSH (tcp/22)" },
  { name: "allowHttp", label: "Allow HTTP", text: "HTTP (tcp/80)" },
  { name: "allowHttps", label: "Allow HTTPS", text: "HTTPS (tcp/443)" },
] as const

type FormControl = Control<
  RunInstancesFormInput,
  unknown,
  CreateInstanceFormData
>

type FormErrors = FieldErrors<RunInstancesFormInput>

type FormRegister = UseFormRegister<RunInstancesFormInput>

interface CountFieldProps {
  control: FormControl
  errors: FormErrors
  instanceTypeCounts: Record<string, number>
  register: FormRegister
}

export function CountField({
  control,
  errors,
  instanceTypeCounts,
  register,
}: CountFieldProps) {
  const selectedInstanceType = useWatch({ control, name: "instanceType" })

  const maxCount = selectedInstanceType
    ? (instanceTypeCounts[selectedInstanceType] ?? 1)
    : 1

  return (
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
}

interface LaunchTemplateSectionProps {
  control: FormControl
  countField: ReactNode
  launchTemplates: readonly LaunchTemplate[]
  selectedTemplateId: string | undefined
}

export function LaunchTemplateSection({
  control,
  countField,
  launchTemplates,
  selectedTemplateId,
}: LaunchTemplateSectionProps) {
  return (
    <PanelSection title="Launch template">
      <div className="flex flex-col gap-4">
        <Field>
          <FieldTitle>
            <label htmlFor="launchTemplateId">Launch Template</label>
            <InfoTip label="Launch template">
              A template supplies the image, instance type, key pair and network
              settings, so those fields are hidden while one is chosen.
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
          <TemplateVersionField
            control={control}
            templateId={selectedTemplateId}
          />
        )}

        {selectedTemplateId && countField}
      </div>
    </PanelSection>
  )
}

function TemplateVersionField({
  control,
  templateId,
}: {
  control: FormControl
  templateId: string
}) {
  const selectedTemplateVersion = useWatch({
    control,
    name: "launchTemplateVersion",
  })

  const templateVersionsQuery = useQuery({
    ...ec2LaunchTemplateVersionsQueryOptions(templateId),
    enabled: !!templateId,
  })

  const templateVersions =
    templateVersionsQuery.data?.LaunchTemplateVersions ?? []

  const resolvedTemplateData = resolveTemplateVersion(
    templateVersions,
    selectedTemplateVersion,
  )?.LaunchTemplateData

  return (
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
  )
}

interface ImageSectionProps {
  arch: Architecture | undefined
  architectures: readonly Architecture[]
  control: FormControl
  errors: FormErrors
  images: Image[]
  onSelectArchitecture: (next: Architecture) => void
  onSelectImage: (imageId: string) => void
}

export function ImageSection({
  arch,
  architectures,
  control,
  errors,
  images,
  onSelectArchitecture,
  onSelectImage,
}: ImageSectionProps) {
  const selectedImageId = useWatch({ control, name: "imageId" })

  return (
    <PanelSection title="Image">
      <div className="flex flex-col gap-4">
        {architectures.length > 1 && (
          <Field>
            <FieldTitle>
              Architecture
              <InfoTip label="Architecture">
                An instance type runs one architecture, so choosing one narrows
                both the images and the instance types to those that can run
                together.
              </InfoTip>
            </FieldTitle>
            <ChoiceTiles columns={2} label="Architecture">
              {architectures.map((candidate) => (
                <ChoiceTile
                  checked={arch === candidate}
                  description={
                    candidate === "arm64" ? "64-bit Arm" : "64-bit x86"
                  }
                  icon={
                    <Cpu aria-hidden className="size-5 text-muted-foreground" />
                  }
                  key={candidate}
                  label={candidate}
                  layout="inline"
                  onSelect={() => {
                    onSelectArchitecture(candidate)
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
            onChange={onSelectImage}
            value={selectedImageId ?? ""}
          />
          <FieldError errors={[errors.imageId]} />
        </Field>
      </div>
    </PanelSection>
  )
}

interface InstanceSectionProps {
  allInstanceTypes: readonly InstanceTypeInfo[]
  arch: Architecture | undefined
  archTypeNames: readonly string[]
  archTypes: InstanceTypeInfo[]
  control: FormControl
  countField: ReactNode
  errors: FormErrors
  hasCompute: boolean
  instanceTypeCounts: Record<string, number>
  keyPairs: readonly KeyPairInfo[]
  placementGroups: readonly PlacementGroup[]
}

export function InstanceSection({
  allInstanceTypes,
  arch,
  archTypeNames,
  archTypes,
  control,
  countField,
  errors,
  hasCompute,
  instanceTypeCounts,
  keyPairs,
  placementGroups,
}: InstanceSectionProps) {
  return (
    <PanelSection title="Instance">
      <div className="flex flex-col gap-4">
        <InstanceTypeField
          allInstanceTypes={allInstanceTypes}
          arch={arch}
          archTypeNames={archTypeNames}
          archTypes={archTypes}
          control={control}
          errors={errors}
          hasCompute={hasCompute}
          instanceTypeCounts={instanceTypeCounts}
        />

        {countField}

        <KeyPairField control={control} errors={errors} keyPairs={keyPairs} />

        <PlacementGroupField
          control={control}
          placementGroups={placementGroups}
        />
      </div>
    </PanelSection>
  )
}

type InstanceTypeFieldProps = Omit<
  InstanceSectionProps,
  "countField" | "keyPairs" | "placementGroups"
>

function InstanceTypeField({
  allInstanceTypes,
  arch,
  archTypeNames,
  archTypes,
  control,
  errors,
  hasCompute,
  instanceTypeCounts,
}: InstanceTypeFieldProps) {
  const selectedInstanceType = useWatch({ control, name: "instanceType" })

  const selectedTypeInfo = allInstanceTypes.find(
    (t) => t.InstanceType === selectedInstanceType,
  )

  const selectedGpuDevice = selectedTypeInfo?.GpuInfo?.Gpus?.[0]

  return (
    <Field>
      <FieldTitle>
        <label htmlFor="instanceType">Instance Type</label>
        <InfoTip label="Instance type">
          vCPUs and memory for the instance, with the free capacity for each
          type. Only types that can run the chosen image&apos;s architecture are
          listed.
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
      {selectedGpuDevice && <GpuDescription device={selectedGpuDevice} />}
      <FieldError errors={[errors.instanceType]} />
    </Field>
  )
}

function GpuDescription({ device }: { device: GpuDeviceInfo }) {
  const migProfile = device.Name?.startsWith("MIG ")
    ? device.Name.slice("MIG ".length)
    : undefined

  return (
    <FieldDescription>
      GPU: {device.Name}
      {device.MemoryInfo?.SizeInMiB !== undefined &&
        ` · ${formatVRAMMiB(device.MemoryInfo.SizeInMiB)} VRAM`}
      {migProfile && ` · MIG ${migProfile} slice`}
    </FieldDescription>
  )
}

function KeyPairField({
  control,
  errors,
  keyPairs,
}: {
  control: FormControl
  errors: FormErrors
  keyPairs: readonly KeyPairInfo[]
}) {
  return (
    <Field>
      <FieldTitle>
        <label htmlFor="keyName">Key Pair</label>
      </FieldTitle>
      <Controller
        control={control}
        name="keyName"
        render={({ field }) => (
          <Select onValueChange={field.onChange} value={field.value ?? ""}>
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
  )
}

function PlacementGroupField({
  control,
  placementGroups,
}: {
  control: FormControl
  placementGroups: readonly PlacementGroup[]
}) {
  return (
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
  )
}

interface NetworkSectionProps {
  control: FormControl
  defaultSubnet: Subnet | undefined
  effectiveSubnet: Subnet | undefined
  effectiveVpcId: string | undefined
  errors: FormErrors
  otherSubnets: readonly Subnet[]
  register: FormRegister
  securityGroups: SecurityGroup[]
  selectedSubnetId: string | undefined
  setValue: UseFormSetValue<RunInstancesFormInput>
}

export function NetworkSection({
  control,
  defaultSubnet,
  effectiveSubnet,
  effectiveVpcId,
  errors,
  otherSubnets,
  register,
  securityGroups,
  selectedSubnetId,
  setValue,
}: NetworkSectionProps) {
  return (
    <PanelSection title="Network">
      <div className="flex flex-col gap-4">
        <Field>
          <FieldTitle>
            Location
            <InfoTip label="Location">
              The region this cluster serves. The availability zone comes from
              the subnet the instance launches into.
            </InfoTip>
          </FieldTitle>
          <LocationTile
            availabilityZone={effectiveSubnet?.AvailabilityZone}
            region={getRegion()}
          />
        </Field>

        <SubnetField
          control={control}
          defaultSubnet={defaultSubnet}
          effectiveSubnet={effectiveSubnet}
          otherSubnets={otherSubnets}
          selectedSubnetId={selectedSubnetId}
        />

        <SecurityGroupField
          control={control}
          effectiveVpcId={effectiveVpcId}
          errors={errors}
          register={register}
          securityGroups={securityGroups}
          setValue={setValue}
        />
      </div>
    </PanelSection>
  )
}

type SubnetFieldProps = Pick<
  NetworkSectionProps,
  | "control"
  | "defaultSubnet"
  | "effectiveSubnet"
  | "otherSubnets"
  | "selectedSubnetId"
>

function SubnetField({
  control,
  defaultSubnet,
  effectiveSubnet,
  otherSubnets,
  selectedSubnetId,
}: SubnetFieldProps) {
  return (
    <Field>
      <FieldTitle>
        <label htmlFor="subnetId">Subnet</label>
        <InfoTip label="Subnet">
          With no subnet chosen, EC2 places the instance in the default
          VPC&apos;s default subnet. It gets a private address there, and other
          instances in the VPC can reach it as its security groups allow. It
          gets a public address only if the subnet assigns one on launch.
        </InfoTip>
      </FieldTitle>
      <Controller
        control={control}
        name="subnetId"
        render={({ field }) => (
          <Select
            onValueChange={(value) => {
              field.onChange(value === DEFAULT_SUBNET ? undefined : value)
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
                <SelectItem key={subnet.SubnetId} value={subnet.SubnetId ?? ""}>
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
  )
}

type SecurityGroupFieldProps = Pick<
  NetworkSectionProps,
  | "control"
  | "effectiveVpcId"
  | "errors"
  | "register"
  | "securityGroups"
  | "setValue"
>

function SecurityGroupField({
  control,
  effectiveVpcId,
  errors,
  register,
  securityGroups,
  setValue,
}: SecurityGroupFieldProps) {
  const sgMode = useWatch({ control, name: "securityGroupMode" })
  const selectedSgIds = useWatch({ control, name: "securityGroupIds" }) ?? []

  const vpcSecurityGroups = effectiveVpcId
    ? securityGroups.filter((sg) => sg.VpcId === effectiveVpcId)
    : securityGroups

  return (
    <Field>
      <FieldTitle>
        Security Group
        <InfoTip label="Security group">
          A new group allows only the inbound ports switched on below. Outbound
          traffic is allowed either way.
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
        <NewSecurityGroupFields
          control={control}
          errors={errors}
          register={register}
        />
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
  )
}

function NewSecurityGroupFields({
  control,
  errors,
  register,
}: {
  control: FormControl
  errors: FormErrors
  register: FormRegister
}) {
  const ruleSource = useWatch({ control, name: "ruleSource" })

  return (
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
        <div className="flex flex-col gap-2">
          {INBOUND_RULE_SWITCHES.map(({ name, label, text }) => (
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
  )
}

interface RootVolumeSectionProps {
  control: FormControl
  errors: FormErrors
  images: readonly Image[]
  register: FormRegister
}

export function RootVolumeSection({
  control,
  errors,
  images,
  register,
}: RootVolumeSectionProps) {
  const selectedImageId = useWatch({ control, name: "imageId" })
  const selectedRoot = imageRootMapping(images, selectedImageId)

  return (
    <PanelSection title="Root volume">
      <div className="flex flex-col gap-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field>
            <FieldTitle>
              <label htmlFor="rootVolumeSize">Size (GiB)</label>
              <InfoTip label="Root volume size">
                Leave blank to use the image&apos;s own size. A larger size
                grows the root volume; it cannot be smaller than the
                image&apos;s snapshot.
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
                  value === "" || value === null || value === undefined
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
                When on, the root volume is deleted with the instance. Turn it
                off to keep the volume after termination.
              </InfoTip>
            </div>
          )}
        />
      </div>
    </PanelSection>
  )
}
