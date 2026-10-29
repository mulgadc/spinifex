import type {
  Image,
  InstanceTypeInfo,
  LaunchTemplateVersion,
} from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import {
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
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { isSystemManagedImage } from "@/lib/system-managed"
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
  createInstanceSchema,
  nextLaunchWizardName,
} from "@/types/ec2"

import {
  ImageField,
  InstanceTypeField,
  KeyPairField,
  LaunchTemplateFields,
  PlacementGroupField,
  RootVolumeFields,
  SecurityGroupFields,
  SubnetField,
} from "./run-instances-fields"

const DEFAULT_ROOT_DEVICE_NAME = "/dev/sda1"
const DEFAULT_SG_CIDR = "0.0.0.0/0"

interface RunInstancesPageProps {
  launchTemplateId?: string
  launchTemplateVersion?: string
}

export function RunInstancesPage({
  launchTemplateId,
  launchTemplateVersion,
}: RunInstancesPageProps) {
  const navigate = useNavigate()
  const createMutation = useCreateInstance()

  const {
    images,
    keyPairs,
    subnets,
    placementGroups,
    securityGroups,
    launchTemplates,
    instanceTypes,
    defaultVpcId,
  } = useLaunchOptions()

  const instanceTypeCounts = countInstanceTypes(instanceTypes)
  const uniqueInstanceTypes = Object.keys(instanceTypeCounts).toSorted()

  const {
    control,
    handleSubmit,
    register,
    setValue,
    formState: { errors, isSubmitting },
  } = useForm({
    resolver: zodResolver(buildRunInstancesSchema(images, instanceTypeCounts)),
    defaultValues: {
      ...launchDefaults(images, keyPairs[0]?.KeyName, uniqueInstanceTypes),
      launchTemplateId,
      launchTemplateVersion: launchTemplateVersion ?? "$Default",
      newSgName: nextLaunchWizardName(
        securityGroups.map((sg) => sg.GroupName ?? ""),
      ),
    },
  })

  const values = useWatch({ control })
  const selectedInstanceType = useWatch({ control, name: "instanceType" })

  const maxCount = selectedInstanceType
    ? (instanceTypeCounts[selectedInstanceType] ?? 1)
    : 1

  const selectedImageId = useWatch({ control, name: "imageId" })

  const selectedRoot = getRootMapping(
    images.find((img) => img.ImageId === selectedImageId),
  )

  const selectedSubnetId = useWatch({ control, name: "subnetId" })

  // Launch template: when one is selected the instance is launched from it and
  // the direct-config fields below are hidden — the template supplies them.
  const selectedTemplateId = useWatch({ control, name: "launchTemplateId" })

  const selectedTemplateVersion = useWatch({
    control,
    name: "launchTemplateVersion",
  })

  const { templateVersions, resolvedTemplateData } = useTemplateVersions(
    selectedTemplateId,
    selectedTemplateVersion,
  )

  const effectiveVpcId =
    subnets.find((s) => s.SubnetId === selectedSubnetId)?.VpcId ?? defaultVpcId

  const vpcSecurityGroups = effectiveVpcId
    ? securityGroups.filter((sg) => sg.VpcId === effectiveVpcId)
    : securityGroups

  const noCompute = uniqueInstanceTypes.length === 0 && !selectedTemplateId
  const isBusy = isSubmitting || createMutation.isPending

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
    navigate({ to: "/ec2/describe-instances" })
  }

  return (
    <>
      <BackLink to="/ec2/describe-instances">Back to instances</BackLink>
      <PageHeading title="Run Instances" />

      {/* Handle error when no instance types available */}
      {noCompute && (
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
        <LaunchTemplateFields
          control={control}
          launchTemplates={launchTemplates}
          resolvedTemplateData={resolvedTemplateData}
          selectedTemplateId={selectedTemplateId}
          templateVersions={templateVersions}
        />

        {!selectedTemplateId && (
          <>
            <ImageField
              control={control}
              errors={errors}
              images={images}
              onImageChange={(imageId) => {
                // Re-prefill DeviceName for the new AMI. Size, type and
                // delete-on-termination stay blank so an unchanged form
                // sends no BlockDeviceMappings.
                setValue(
                  "rootDeviceName",
                  getRootMapping(images.find((img) => img.ImageId === imageId))
                    .deviceName,
                )
              }}
            />
            <InstanceTypeField
              control={control}
              errors={errors}
              instanceTypeCounts={instanceTypeCounts}
              instanceTypes={instanceTypes}
              selectedInstanceType={selectedInstanceType}
            />
            <KeyPairField
              control={control}
              errors={errors}
              keyPairs={keyPairs}
            />
            <SubnetField control={control} subnets={subnets} />
            <SecurityGroupFields
              control={control}
              effectiveVpcId={effectiveVpcId}
              errors={errors}
              register={register}
              setValue={setValue}
              vpcSecurityGroups={vpcSecurityGroups}
            />
            <PlacementGroupField
              control={control}
              placementGroups={placementGroups}
            />
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
          <RootVolumeFields
            control={control}
            errors={errors}
            register={register}
            snapshotSize={selectedRoot.snapshotSize}
          />
        )}

        <CliCommandPanel
          commands={buildRunInstancesCommands(values, effectiveVpcId)}
        />

        {/* Actions */}
        <div className="flex gap-2">
          <Button
            disabled={isBusy}
            onClick={async () => {
              await navigate({ to: "/ec2/describe-instances" })
            }}
            type="button"
            variant="outline"
          >
            Cancel
          </Button>
          <Button disabled={isBusy || noCompute} type="submit">
            {isBusy ? "Creating…" : "Create Instance"}
          </Button>
        </div>
      </form>
    </>
  )
}

// useLaunchOptions loads every resource list the wizard offers. System-managed
// images are hidden; the default VPC (or the first one) backs an unset subnet.
function useLaunchOptions() {
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
  const vpcs = vpcsData.Vpcs ?? []

  return {
    images: (imagesData.Images ?? []).filter(
      (image) => !isSystemManagedImage(image),
    ),
    keyPairs: keyPairsData.KeyPairs ?? [],
    subnets: subnetsData.Subnets ?? [],
    placementGroups: pgData.PlacementGroups ?? [],
    securityGroups: sgData.SecurityGroups ?? [],
    launchTemplates: ltData.LaunchTemplates ?? [],
    instanceTypes: instanceTypesData.InstanceTypes ?? [],
    defaultVpcId: (vpcs.find((v) => v.IsDefault) ?? vpcs[0])?.VpcId,
  }
}

function useTemplateVersions(
  templateId: string | undefined,
  selectedVersion: string | undefined,
) {
  const templateVersionsQuery = useQuery({
    ...ec2LaunchTemplateVersionsQueryOptions(templateId ?? ""),
    enabled: !!templateId,
  })

  const templateVersions =
    templateVersionsQuery.data?.LaunchTemplateVersions ?? []

  return {
    templateVersions,
    resolvedTemplateData: resolveTemplateVersion(
      templateVersions,
      selectedVersion,
    )?.LaunchTemplateData,
  }
}

function countInstanceTypes(instanceTypes: InstanceTypeInfo[]) {
  const counts: Record<string, number> = {}

  for (const type of instanceTypes) {
    const typeName = type.InstanceType

    if (!typeName) {
      continue
    }

    counts[typeName] = (counts[typeName] ?? 0) + 1
  }

  return counts
}

function buildRunInstancesSchema(
  images: Image[],
  instanceTypeCounts: Record<string, number>,
) {
  return createInstanceSchema
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
    })
}

// launchDefaults prefills the direct-launch fields from the loaded data: the
// first image and key pair, and a nano instance type when one is offered.
function launchDefaults(
  images: Image[],
  defaultKeyName: string | undefined,
  uniqueInstanceTypes: string[],
): Partial<CreateInstanceFormData> {
  const defaultImageId = images[0]?.ImageId

  const defaultInstanceType =
    uniqueInstanceTypes.find((type) => type.endsWith(".nano")) ??
    uniqueInstanceTypes[0]

  const defaultRoot = getRootMapping(
    images.find((img) => img.ImageId === defaultImageId),
  )

  return {
    count: 1,
    imageId: defaultImageId ?? "",
    keyName: defaultKeyName ?? "",
    instanceType: defaultInstanceType ?? "",
    rootDeviceName: defaultRoot.deviceName,
    securityGroupMode: "create",
    securityGroupIds: [],
    newSgDescription: "Created by launch wizard",
    allowSsh: true,
    allowHttp: false,
    allowHttps: false,
    ruleSource: "anywhere",
    customCidr: "",
  }
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

  const countFlag = commandFlag("\\\n  --count", (values.count ?? 0) || 1)

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
          ...countFlag,
        ],
      },
    ]
  }

  const securityGroups = buildSecurityGroupArgs(values, vpcId)

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
    ...countFlag,
    ...securityGroups.flags,
    ...rootVolumeFlags(values),
  ]

  return [...securityGroups.commands, { label: "Run Instances", parts }]
}

interface SecurityGroupArgs {
  commands: CliCommand[]
  flags: CommandPart[]
}

// buildSecurityGroupArgs: create-new emits create + authorize commands and
// feeds $SG_ID into run-instances; select-existing passes the chosen IDs.
function buildSecurityGroupArgs(
  values: DeepPartialSkipArrayKey<CreateInstanceFormData>,
  vpcId: string | undefined,
): SecurityGroupArgs {
  if ((values.securityGroupMode || "create") !== "create") {
    return {
      commands: [],
      flags: optionalFlag(
        "\\\n  --security-group-ids",
        values.securityGroupIds?.join(" "),
      ),
    }
  }

  const commands: CliCommand[] = [
    {
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
    },
  ]

  const ingress = buildIngressCommand(values)

  if (ingress) {
    commands.push(ingress)
  }

  return {
    commands,
    flags: commandFlag("\\\n  --security-group-ids", "$SG_ID"),
  }
}

function buildIngressCommand(
  values: DeepPartialSkipArrayKey<CreateInstanceFormData>,
): CliCommand | undefined {
  const cidr =
    values.ruleSource === "custom" && values.customCidr
      ? values.customCidr
      : DEFAULT_SG_CIDR

  const ports = [
    ...(values.allowSsh === true ? [22] : []),
    ...(values.allowHttp === true ? [80] : []),
    ...(values.allowHttps === true ? [443] : []),
  ]

  if (ports.length === 0) {
    return undefined
  }

  const ipPerms = JSON.stringify(
    ports.map((port) => ({
      IpProtocol: "tcp",
      FromPort: port,
      ToPort: port,
      IpRanges: [{ CidrIp: cidr }],
    })),
  )

  return {
    label: "Authorize inbound rules",
    parts: [
      {
        type: "bin",
        value: "AWS_PROFILE=spinifex aws ec2 authorize-security-group-ingress",
      },
      ...commandFlag("\\\n  --group-id", "$SG_ID"),
      ...commandFlag("\\\n  --ip-permissions", `'${ipPerms}'`),
    ],
  }
}

function rootVolumeFlags(
  values: DeepPartialSkipArrayKey<CreateInstanceFormData>,
): CommandPart[] {
  const { rootDeleteOnTermination, rootVolumeSize, rootVolumeType } = values

  const hasStorageOverride =
    rootVolumeSize !== undefined ||
    rootVolumeType !== undefined ||
    rootDeleteOnTermination !== undefined

  if (!hasStorageOverride) {
    return []
  }

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

  return commandFlag("\\\n  --block-device-mappings", `'${bdm}'`)
}
