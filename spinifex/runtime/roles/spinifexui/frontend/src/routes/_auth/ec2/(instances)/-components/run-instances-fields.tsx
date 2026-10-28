import type {
  Image,
  InstanceTypeInfo,
  KeyPairInfo,
  LaunchTemplate,
  LaunchTemplateVersion,
  PlacementGroup,
  ResponseLaunchTemplateData,
  SecurityGroup,
  Subnet,
} from "@aws-sdk/client-ec2"
import { ChevronDown } from "lucide-react"
import {
  type Control,
  Controller,
  type FieldErrors,
  type UseFormRegister,
  type UseFormSetValue,
  useWatch,
} from "react-hook-form"

import { GpuInstanceTypeSelect } from "@/components/gpu-instance-type-select"
import { SecurityGroupCheckboxes } from "@/components/security-group-checkboxes"
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
import { formatVRAMMiB } from "@/lib/utils"
import { type CreateInstanceFormData, VOLUME_TYPES } from "@/types/ec2"

const DEFAULT_VOLUME_TYPE = "gp3"
const MIN_VOLUME_SIZE_GIB = 1
const MAX_VOLUME_SIZE_GIB = 16_384
const MIG_PREFIX = "MIG "

const INBOUND_RULE_CHECKBOXES = [
  { name: "allowSsh", label: "Allow SSH", text: "Allow SSH (tcp/22)" },
  { name: "allowHttp", label: "Allow HTTP", text: "Allow HTTP (tcp/80)" },
  { name: "allowHttps", label: "Allow HTTPS", text: "Allow HTTPS (tcp/443)" },
] as const

type FormControl = Control<CreateInstanceFormData>

type FormErrors = FieldErrors<CreateInstanceFormData>

type FormRegister = UseFormRegister<CreateInstanceFormData>

interface LaunchTemplateFieldsProps {
  control: FormControl
  launchTemplates: LaunchTemplate[]
  templateVersions: LaunchTemplateVersion[]
  resolvedTemplateData: ResponseLaunchTemplateData | undefined
  selectedTemplateId: string | undefined
}

export function LaunchTemplateFields({
  control,
  launchTemplates,
  templateVersions,
  resolvedTemplateData,
  selectedTemplateId,
}: LaunchTemplateFieldsProps) {
  return (
    <>
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
            <TemplateDataSummary data={resolvedTemplateData} />
          )}
        </Field>
      )}
    </>
  )
}

function TemplateDataSummary({ data }: { data: ResponseLaunchTemplateData }) {
  return (
    <div className="mt-3 space-y-1 rounded-md border border-border p-3 text-xs">
      <p className="text-muted-foreground">Configuration from this version:</p>
      <p>
        Image: <span className="font-mono">{data.ImageId ?? "—"}</span>
      </p>
      <p>
        Instance type:{" "}
        <span className="font-mono">{data.InstanceType ?? "—"}</span>
      </p>
      <p>
        Key pair: <span className="font-mono">{data.KeyName ?? "—"}</span>
      </p>
    </div>
  )
}

interface ImageFieldProps {
  control: FormControl
  errors: FormErrors
  images: Image[]
  onImageChange: (imageId: string) => void
}

export function ImageField({
  control,
  errors,
  images,
  onImageChange,
}: ImageFieldProps) {
  return (
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
                onImageChange(value ?? "")
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
                  <SelectItem key={image.ImageId} value={image.ImageId ?? ""}>
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
  )
}

interface InstanceTypeFieldProps {
  control: FormControl
  errors: FormErrors
  instanceTypes: InstanceTypeInfo[]
  instanceTypeCounts: Record<string, number>
  selectedInstanceType: string | undefined
}

export function InstanceTypeField({
  control,
  errors,
  instanceTypes,
  instanceTypeCounts,
  selectedInstanceType,
}: InstanceTypeFieldProps) {
  const selectedTypeInfo = instanceTypes.find(
    (t) => t.InstanceType === selectedInstanceType,
  )

  return (
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
            instanceTypes={instanceTypes}
            onValueChange={field.onChange}
            value={field.value ?? ""}
          />
        )}
      />
      <GpuDescription typeInfo={selectedTypeInfo} />
      <FieldError errors={[errors.instanceType]} />
    </Field>
  )
}

function GpuDescription({
  typeInfo,
}: {
  typeInfo: InstanceTypeInfo | undefined
}) {
  const gpu = typeInfo?.GpuInfo?.Gpus?.[0]

  if (!gpu) {
    return null
  }

  const migProfile = gpu.Name?.startsWith(MIG_PREFIX)
    ? gpu.Name.slice(MIG_PREFIX.length)
    : undefined

  return (
    <FieldDescription>
      GPU: {gpu.Name}
      {gpu.MemoryInfo?.SizeInMiB !== undefined &&
        ` · ${formatVRAMMiB(gpu.MemoryInfo.SizeInMiB)} VRAM`}
      {migProfile && ` · MIG ${migProfile} slice`}
    </FieldDescription>
  )
}

interface KeyPairFieldProps {
  control: FormControl
  errors: FormErrors
  keyPairs: KeyPairInfo[]
}

export function KeyPairField({ control, errors, keyPairs }: KeyPairFieldProps) {
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

interface SubnetFieldProps {
  control: FormControl
  subnets: Subnet[]
}

export function SubnetField({ control, subnets }: SubnetFieldProps) {
  return (
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
                <SelectItem key={subnet.SubnetId} value={subnet.SubnetId ?? ""}>
                  {subnet.SubnetId}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        )}
      />
    </Field>
  )
}

interface SecurityGroupFieldsProps {
  control: FormControl
  register: FormRegister
  errors: FormErrors
  setValue: UseFormSetValue<CreateInstanceFormData>
  effectiveVpcId: string | undefined
  vpcSecurityGroups: SecurityGroup[]
}

export function SecurityGroupFields({
  control,
  register,
  errors,
  setValue,
  effectiveVpcId,
  vpcSecurityGroups,
}: SecurityGroupFieldsProps) {
  const sgMode = useWatch({ control, name: "securityGroupMode" })
  const selectedSgIds = useWatch({ control, name: "securityGroupIds" }) ?? []

  return (
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
        <NewSecurityGroupFields
          control={control}
          errors={errors}
          register={register}
        />
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
  )
}

interface NewSecurityGroupFieldsProps {
  control: FormControl
  register: FormRegister
  errors: FormErrors
}

function NewSecurityGroupFields({
  control,
  register,
  errors,
}: NewSecurityGroupFieldsProps) {
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
        <div className="space-y-1">
          {INBOUND_RULE_CHECKBOXES.map(({ name, label, text }) => (
            <label className="flex items-center gap-2 text-sm" key={name}>
              <input aria-label={label} type="checkbox" {...register(name)} />
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
  )
}

interface PlacementGroupFieldProps {
  control: FormControl
  placementGroups: PlacementGroup[]
}

export function PlacementGroupField({
  control,
  placementGroups,
}: PlacementGroupFieldProps) {
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

interface RootVolumeFieldsProps {
  control: FormControl
  register: FormRegister
  errors: FormErrors
  snapshotSize: number | undefined
}

// RootVolumeFields is collapsed by default; size, type and
// delete-on-termination stay blank so an unchanged form sends no
// BlockDeviceMappings.
export function RootVolumeFields({
  control,
  register,
  errors,
  snapshotSize,
}: RootVolumeFieldsProps) {
  return (
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
            min={snapshotSize ?? MIN_VOLUME_SIZE_GIB}
            placeholder={
              snapshotSize
                ? `${snapshotSize} (AMI default)`
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
          {snapshotSize !== undefined && (
            <FieldDescription>
              AMI snapshot size: {snapshotSize} GiB
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
  )
}
